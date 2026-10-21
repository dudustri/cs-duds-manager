// Shared helpers for the stack-tier Playwright specs.
//
// UI CONTRACT the lobby (web/index.html + src/main.ts) must satisfy, as these specs use it:
//   [data-testid=name-input]                    player name field (applied to the engine on connect)
//   header nav [data-testid=go-servers] / go-players / go-create: the three pages (/, /#players, /#create)
//   [data-testid=stat-players] / stat-servers / stat-maps   hero numbers; [data-testid=quick-play][data-mode=M]
//                                               joins the fullest open server of mode M, or starts one and joins
//   [data-testid=list-search] / list-mode / list-sort   filter and order the server list
//   [data-testid=view-list] / view-cards        list (table, default) or cards ([data-testid=room-cards], same
//                                               [data-room-id] + [data-testid=connect] inside each card)
//   [data-testid=people]                        /#players: one row per player online
//   [data-testid=no-rooms]                      lobby: shown instead of the list while no room is running
//   on /#create:
//     [data-testid=map-search] / map-select     search box narrowing the map <select> (filled from /api/catalog)
//     [data-testid=mode-option][data-mode=M]    one card per mode (click to choose; classic is preselected)
//     [data-testid=create-room]                 creates the room (does NOT join); then [data-testid=toast] shows
//       [data-testid=toast-join]                joins it as host; [data-testid=toast-url] is the invite link (/#room/ID)
//   [data-testid=room-list] [data-room-id=ID]   one row per room: text has map, mode and "players/maxPlayers" ("0/16")
//     [data-testid=connect]                     in a row: loads bundles, boots the engine, joins
//   [data-testid=progress]                      visible while bundles download
//   [data-testid=status]                        connection state text; contains "disconnected" after a kick
//   [data-testid=leave]                         leaves the room
//   [data-testid=host-panel]                    only in the host's browser: [data-testid=player-row] per player (text = name)
//     [data-testid=kick]                        inside a player-row
//     [data-testid=vote-map-option][value=MAP]  checkboxes (narrowed by [data-testid=vote-search]); vote-start posts vote-map
//   [data-testid=max-players-option]            room-size chips 4/6/8/12/16/20/32 (radios; 16 checked) (Amendment A)
//   [data-testid=max-players-custom]            custom room size 2..32; overrides the chips while filled
//   [data-testid=game]                          the game container (canvas inside); F11 fullscreens THIS element
//   [data-testid=game-overlay]                  inside game: visible while the pointer is NOT locked, hidden while
//                                               locked; holds leave, fullscreen and (for the host) the host panel
//     [data-testid=fullscreen]                  toggles fullscreen of the game container
//   [data-testid=loading-screen]                covers the game while joining; [data-testid=loading-stage] text is
//                                               "download…" → "engine…" → "joining…" (case-insensitive substrings);
//                                               hidden once in game. Connect requests fullscreen on [data-testid=game]
//                                               inside the click (user gesture).
//   GET /?boot                                  loads bundles and boots the engine to the menu WITHOUT connecting
//   window.xash                                 the Xash3D instance (Cmd_ExecuteString, getCVar, ...) once booted
import { chromium, expect, test } from '@playwright/test';
import type { BrowserContext, Page } from '@playwright/test';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

export const STACK = process.env.STACK_URL ?? '';
export const CHROMIUM_ARGS = ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'];
export const ADMIN_PASSWORD = 'test-admin-pw';

/** Call at the top of a spec file: skips every test when no stack is running. */
export function requireStack(): void {
  test.skip(!STACK, 'STACK_URL not set; run scripts/test stack');
}

/** ReHLDS keeps ghosts for sv_timeout, so every player needs a fresh name (<= 31 chars). */
export const uniqueName = (prefix: string): string =>
  `${prefix}${Date.now().toString(36)}${Math.random().toString(36).slice(2, 5)}`.slice(0, 31);

/** A persistent Chromium context with its own empty on-disk profile. */
export async function profile(viewport = { width: 320, height: 240 }): Promise<BrowserContext> {
  const dir = mkdtempSync(join(tmpdir(), 'cs-pw-'));
  const ctx = await chromium.launchPersistentContext(dir, { baseURL: STACK, args: CHROMIUM_ARGS, viewport });
  ctx.on('close', () => rmSync(dir, { recursive: true, force: true })); // each profile caches ~350 MB of bundles
  return ctx;
}

export interface Room { id: string; map: string; mode: string; players: number; maxPlayers: number; hostToken?: string }
export interface ApiPlayer { userid: number; name: string; steamid: string; frags: number; ping: number; bot: boolean }

async function json<T>(res: Response): Promise<T> {
  if (!res.ok) throw new Error(`${res.url}: ${res.status} ${await res.text()}`);
  return (await res.json()) as T;
}

export const api = {
  createRoom: (map: string, mode: string) =>
    fetch(`${STACK}/api/rooms`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ map, mode }) }).then((r) => json<Room>(r)),
  listRooms: () => fetch(`${STACK}/api/rooms`).then((r) => json<Room[]>(r)),
  players: (id: string, hostToken: string) =>
    fetch(`${STACK}/api/rooms/${id}/players`, { headers: { authorization: `Bearer ${hostToken}` } }).then((r) => json<ApiPlayer[]>(r)),
  async deleteRoom(id: string) {
    const login = await fetch(`${STACK}/api/admin/login`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ password: ADMIN_PASSWORD }) });
    const cookie = (login.headers.get('set-cookie') ?? '').split(';')[0];
    await fetch(`${STACK}/api/rooms/${id}`, { method: 'DELETE', headers: { cookie } });
  },
};

/** Engine console output of a page (the engine logs to console.*). */
export function captureLog(page: Page): string[] {
  const lines: string[] = [];
  page.on('console', (m) => lines.push(m.text()));
  return lines;
}

/** Lobby flow: open the list, set the name, click Connect on the room's row. */
export async function joinFromList(page: Page, roomId: string, name: string): Promise<void> {
  await page.goto('/');
  await setPlayerName(page, name);
  await page.locator(`[data-room-id="${roomId}"]`).getByTestId('connect').click();
}

/** Types the name and claims it with Set (names are unique; the game plays under the claimed one). */
export async function setPlayerName(page: Page, name: string): Promise<void> {
  const box = page.getByTestId('name-input');
  if ((await box.inputValue()) === name) return; // already ours: this profile claimed it on an earlier load
  await box.fill(name);
  await page.getByTestId('name-set').click();
  await expect(page.getByTestId('name-set')).toBeHidden();
  await expect(box).toHaveValue(name);
}

/** Polls the host API until exactly these names are listed (any order); returns the players. */
export async function waitForPlayers(room: Room, names: string[], timeout = 180_000): Promise<ApiPlayer[]> {
  let last: ApiPlayer[] = [];
  await expect
    .poll(async () => (last = await api.players(room.id, room.hostToken!)).map((p) => p.name).sort(), { timeout, intervals: [1000] })
    .toEqual([...names].sort());
  return last;
}

export const xash = (page: Page, cmd: string) => page.evaluate((c) => (window as any).xash.Cmd_ExecuteString(c), cmd);
