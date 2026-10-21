// Phase 4 exit: create -> join -> vote map -> kick -> idle reap, through the UI.
import { expect, test } from '@playwright/test';
import { api, captureLog, joinFromList, profile, requireStack, setPlayerName, uniqueName, waitForPlayers, xash } from './fixtures.ts';

requireStack();

test('host creates a room, votes a map, kicks a player; empty room is reaped', async () => {
  const hostCtx = await profile();
  const guestCtx = await profile();
  let roomId = '';
  try {
    const host = await hostCtx.newPage();
    const log = captureLog(host);
    const hostName = uniqueName('host');
    const before = new Set((await api.listRooms()).map((r) => r.id));

    await host.goto('/#create'); // the create page
    await setPlayerName(host, hostName);
    await host.getByTestId('map-select').selectOption('de_dust2');
    await host.locator('[data-testid=mode-option][data-mode=classic]').click();
    await host.getByTestId('create-room').click();
    await expect.poll(async () => (await api.listRooms()).find((r) => !before.has(r.id))?.id ?? '', { timeout: 120_000 }).not.toBe('');
    roomId = (await api.listRooms()).find((r) => !before.has(r.id))!.id;
    await host.getByTestId('toast-join').click({ timeout: 60_000 }); // creating doesn't join; the toast offers it

    // auto-joined, and the host panel lists us
    const panel = host.getByTestId('host-panel');
    await expect(panel.getByTestId('player-row').filter({ hasText: hostName })).toBeVisible({ timeout: 300_000 });

    // vote map: de_inferno
    await panel.locator('[data-testid=vote-map-option][value=de_inferno]').check();
    await panel.getByTestId('vote-start').click();
    await xash(host, 'menuselect 1');
    await expect.poll(async () => (await api.listRooms()).find((r) => r.id === roomId)?.map, { timeout: 120_000 }).toBe('de_inferno');
    // the client followed the changelevel: the server's `status` reply names the map (the engine logs no map name by default)
    await expect.poll(async () => (await xash(host, 'status'), log.some((l) => /map\s*:\s*de_inferno/.test(l))), { timeout: 60_000 }).toBe(true);
    await expect(panel.getByTestId('player-row').filter({ hasText: hostName })).toBeVisible({ timeout: 60_000 }); // survived changelevel

    // a second player joins; the host kicks it
    const guest = await guestCtx.newPage();
    const guestName = uniqueName('guest');
    await joinFromList(guest, roomId, guestName);
    const row = panel.getByTestId('player-row').filter({ hasText: guestName });
    await expect(row).toBeVisible({ timeout: 300_000 });
    await row.getByTestId('kick').click();
    await expect(row).toHaveCount(0, { timeout: 30_000 });
    await expect(guest.getByTestId('status')).toContainText(/disconnected/i, { timeout: 30_000 });

    // everyone leaves -> idle reap (ROOM_IDLE_TIMEOUT 45 s in the test stack, plus two polls)
    await host.getByTestId('leave').click();
    await expect.poll(async () => (await api.listRooms()).some((r) => r.id === roomId), { timeout: 45_000 + 2 * 5_000 + 30_000, intervals: [2000] }).toBe(false);
  } finally {
    await hostCtx.close();
    await guestCtx.close();
    if (roomId) await api.deleteRoom(roomId);
  }
});
