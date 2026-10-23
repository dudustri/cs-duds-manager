// The game view: one phase (lobby -> loading -> playing -> lobby) that decides what is on screen, plus fullscreen,
// pointer lock, the engine and the room connection.
import * as api from './api.ts';
import { loadBundles } from './assets.ts';
import { bootEngine, mountMaps, setName, type Engine } from './engine.ts';
import { myName, renewName } from './name.ts';
import { CONNECT_CMD, RelayTransport } from './transport.ts';
import { $, errorText, setNote } from './ui.ts';

export type Phase = 'lobby' | 'loading' | 'playing';

declare const __LOADING_IMAGES__: string[]; // set by scripts/build.mjs from web/img/loading/

export const state = {
  phase: 'lobby' as Phase,
  roomId: undefined as string | undefined, // the room we are connected to (or joining)
  leftRoomId: undefined as string | undefined, // the room we last left, while the server may still list us in it
  isAdmin: false,
};

const lobby = $('lobby');
const game = $('game');
const canvas = $<HTMLCanvasElement>('canvas');
const loading = $('loading');
const overlay = $('overlay');

const transport = new RelayTransport();
let engine: Engine | undefined;
let booting: Promise<Engine> | undefined;
let epoch = 0; // bumped on every move to loading or lobby, so a late async step can tell it was cancelled
let wantLock = false; // the player clicked the canvas; the engine's own pointer-lock requests don't count

const listeners: (() => void)[] = [];
/** `run` is called after every phase change and whenever the overlay opens. */
export const onChange = (run: () => void) => void listeners.push(run);

export const overlayOpen = () => !overlay.hidden;
const fullscreen = () => document.fullscreenElement === game;

function syncOverlay(): void {
  overlay.hidden = !(state.phase === 'playing' && !(wantLock && document.pointerLockElement));
  listeners.forEach((run) => run());
}

/** The only place the screen changes between lobby, loading screen and game. Returns the new epoch. */
function setPhase(next: Phase): number {
  if (next !== 'playing') epoch++;
  state.phase = next;
  lobby.hidden = next !== 'lobby';
  game.hidden = next === 'lobby';
  loading.hidden = next !== 'loading';
  game.classList.toggle('is-loading', next === 'loading');
  if (next === 'lobby') {
    state.leftRoomId = state.roomId ?? state.leftRoomId;
    state.roomId = undefined;
    if (document.fullscreenElement) void document.exitFullscreen().catch(() => {});
    if (document.pointerLockElement) document.exitPointerLock();
  }
  syncOverlay();
  return epoch;
}

function renderFullscreenButton(): void {
  $('fullscreen').innerHTML = fullscreen()
    ? '<span>Exit full screen</span>'
    : '<span>⛶ Full screen</span><small>Press F11 to go back to full screen</small>';
}

// Must run inside a user gesture. If the browser refuses, #game still fills the viewport through CSS.
const requestFullscreen = () => void game.requestFullscreen?.()?.catch(() => {});
function toggleFullscreen(): void {
  if (fullscreen()) void document.exitFullscreen();
  else requestFullscreen();
}

const STEPS = ['download', 'engine', 'join'];
export function setStage(step: string, text: string, fraction?: number): void {
  $('load-stage').textContent = text;
  const at = STEPS.indexOf(step);
  [...$('steps').children].forEach((li, i) => {
    li.classList.toggle('done', i < at);
    li.classList.toggle('now', i === at);
  });
  $('bar').style.width = fraction === undefined ? '' : `${Math.round(fraction * 100)}%`;
  $('bar').classList.toggle('indeterminate', fraction === undefined);
}

/** Call synchronously from the click handler: the game takes over the screen at once (fullscreen needs the gesture). */
export function enterGame(mapName: string, sub: string): number {
  const my = setPhase('loading');
  const bg = __LOADING_IMAGES__[Math.floor(Math.random() * __LOADING_IMAGES__.length)];
  if (bg) loading.style.setProperty('--load-img', `url("${bg}")`);
  $('load-map').textContent = mapName;
  $('load-sub').textContent = sub;
  setStage('download', 'Preparing');
  requestFullscreen();
  return my;
}

export const isCurrent = (my: number) => epoch === my;

export function backToLobby(status: string, bad: boolean): void {
  setPhase('lobby');
  setNote('status', status, bad);
}

document.addEventListener('keydown', (ev) => {
  if (ev.key !== 'F11' || state.phase === 'lobby') return;
  ev.preventDefault();
  toggleFullscreen();
});
// The engine asks for the mouse by itself as soon as it is in game; only a click by the player may grant it,
// otherwise the overlay (and Leave) would be unreachable. Capture phase: runs before the engine's own handlers.
const grantLock = canvas.requestPointerLock.bind(canvas);
canvas.requestPointerLock = (...args) => (wantLock ? grantLock(...args) : Promise.resolve());
canvas.addEventListener('pointerdown', () => (wantLock = state.phase === 'playing'), true);
canvas.addEventListener('click', () => {
  if (wantLock) Promise.resolve(canvas.requestPointerLock()).catch(() => {});
});
document.addEventListener('pointerlockchange', () => {
  if (!document.pointerLockElement) wantLock = false;
  syncOverlay();
});
document.addEventListener('fullscreenchange', () => {
  const kb = (navigator as unknown as { keyboard?: { lock(keys: string[]): Promise<void>; unlock(): void } }).keyboard;
  if (fullscreen()) void kb?.lock(['Escape']).catch(() => {}); // Chromium only: a short Esc reaches the CS menu
  else {
    kb?.unlock();
    if (document.pointerLockElement) document.exitPointerLock(); // holding Esc left fullscreen: free the mouse too
  }
  renderFullscreenButton();
});
$('fullscreen').onclick = toggleFullscreen;
renderFullscreenButton();

// ---- Engine and connection -------------------------------------------------------------------------------------

const showDownload = (d: number, t: number, step?: 'checking' | 'saving') =>
  setStage(
    'download',
    step === 'checking' ? 'Checking the downloaded files' : step === 'saving' ? 'Saving the files for next time'
      : `Downloading game files (${Math.round(d / 1e6)} / ${Math.round(t / 1e6)} MB)`,
    d / t,
  );

// Engine boot, once per page: the base bundle and the first map, then the wasm engine.
function boot(firstMap: string): Promise<Engine> {
  booting ??= (async () => {
    // The engine takes its resolution from the canvas attributes once, at boot (it can't change it later), and
    // the game runs fullscreen: so boot at the screen's size; CSS scales it down in a smaller window.
    canvas.width = Math.max(320, screen.width);
    canvas.height = Math.max(240, screen.height);
    setStage('download', 'Downloading game files');
    const bundles = await loadBundles(firstMap ? [firstMap] : [], true, showDownload);
    setStage('engine', 'Starting engine');
    engine = await bootEngine(canvas, bundles, transport, onEngineLog);
    if (firstMap) mapsMounted.set(firstMap, Promise.resolve());
    return engine;
  })();
  booting.catch(() => (booting = undefined));
  return booting;
}

// Maps are fetched one at a time: on join, and when a map change finds the next one missing (see onEngineLog).
const mapsMounted = new Map<string, Promise<void>>();
function ensureMap(x: Engine, map: string, onProgress?: (d: number, t: number) => void): Promise<void> {
  let p = mapsMounted.get(map);
  if (!p) {
    p = loadBundles([map], false, onProgress ?? (() => {})).then((b) => mountMaps(x, b));
    p.catch(() => mapsMounted.delete(map));
    mapsMounted.set(map, p);
  }
  return p;
}
const MISSING_MAP = /Could not load model maps\/([a-z0-9_.-]+)\.bsp/i;

/** A map change to a map we don't have: fetch it behind the loading screen, then reconnect. */
async function fetchMapAndRejoin(map: string): Promise<void> {
  const x = engine;
  if (!x || !state.roomId) return;
  const my = setPhase('loading');
  newMapTeam();
  $('load-map').textContent = map;
  $('load-sub').textContent = 'Map change';
  setStage('download', 'Downloading map');
  try {
    await ensureMap(x, map, showDownload);
    if (!isCurrent(my)) return;
    setStage('join', 'Joining server');
    x.Cmd_ExecuteString(CONNECT_CMD);
    setTimeout(() => isCurrent(my) && state.phase === 'loading' && leave('Could not rejoin after the map change.', true), 45_000);
  } catch (e) {
    if (isCurrent(my)) leave(`Could not load ${map}: ${errorText(e)}`, true);
  }
}

// The relay keeps the channel open when the server drops us, so the engine's own log tells us. Only svc_disconnect
// means *we* were dropped; "Server disconnected, reconnecting" (changelevel) and "<name> was kicked" do not.
const DROPPED = /Server issued disconnect\. Reason: (.*)/;
// the engine reloads the game folders for every new map (it also does at boot, while we are still loading)
const CHANGING = /FS_AddGameHierarchy\( valve \)|Server (changing|disconnected), reconnecting/;
const IN_GAME = 'cs-duds: in game'; // echoed by our server plugin (cs_duds_votes.sma) once the map has loaded
function onEngineLog(line: string): void {
  if (line.includes(IN_GAME) && state.phase === 'loading' && state.roomId) {
    return void setPhase('playing');
  }
  if (CHANGING.test(line) && state.phase === 'playing') return void showMapChange();
  const missing = MISSING_MAP.exec(line);
  if (missing && state.roomId && state.phase !== 'lobby') return void fetchMapAndRejoin(missing[1].toLowerCase());
  const m = DROPPED.exec(line);
  if (m && state.roomId && state.phase !== 'lobby') leave(`Disconnected: ${m[1].trim().slice(0, 100)}`, true);
}

/** CS asks for a team on every new map: free the mouse so the team picker can show once we are in. */
function newMapTeam(): void {
  teamChosen = false;
  wantLock = false;
  if (document.pointerLockElement) document.exitPointerLock();
}

/** A level change: our loading screen instead of the engine's console until the next map is in, then a team again. */
function showMapChange(): void {
  const my = setPhase('loading');
  $('load-map').textContent = 'Next map';
  $('load-sub').textContent = 'Map change';
  setStage('join', 'Loading the next map');
  newMapTeam();
  setTimeout(() => isCurrent(my) && state.phase === 'loading' && leave('Could not join the next map.', true), 90_000);
}

const hostToken = () => (state.roomId ? api.hostTokens.get(state.roomId) : undefined);
export const canManage = () => Boolean(hostToken()) || state.isAdmin;
/** Host token, else the session token (players), else the admin cookie. */
export const auth = (): api.Auth =>
  hostToken() ? api.hostAuth(hostToken()) : transport.sessionToken ? api.sessionAuth(transport.sessionToken) : undefined;

export async function join(id: string, map: string, my: number): Promise<void> {
  state.roomId = id;
  teamChosen = false;
  renewName();
  try {
    const x = await boot(map);
    if (!isCurrent(my)) return;
    await ensureMap(x, map, showDownload);
    if (!isCurrent(my)) return;
    setStage('join', 'Joining server');
    setNote('status', '');
    setName(x, myName());
    await transport.open(id, api.hostAuth(hostToken()));
    if (!isCurrent(my)) return transport.close();
    x.Cmd_ExecuteString('_vgui_menus 0'); // text menus: the server closes them itself when the overlay picks a team
    x.Cmd_ExecuteString(CONNECT_CMD);
    if (hostToken()) x.Cmd_ExecuteString('bind F3 amxmodmenu');
    setTimeout(() => isCurrent(my) && state.phase === 'loading' && leave('Could not join: the server did not let us in.', true), 45_000);
  } catch (e) {
    if (!isCurrent(my)) return;
    transport.close();
    backToLobby(`Could not join: ${errorText(e)}`, true);
  }
}

let teamChosen = false;
/** Joins a team from the overlay (1 T, 2 CT, 5 auto, 6 spectator) and, since this is a click, takes the mouse. */
export function pickTeam(team: number): void {
  if (!engine || state.phase !== 'playing') return;
  engine.Cmd_ExecuteString(`jointeam ${team}`);
  if (team !== 6) engine.Cmd_ExecuteString('joinclass 5'); // 5: a random skin of that team
  teamChosen = true;
  wantLock = true;
  Promise.resolve(grantLock()).catch(() => {});
  syncOverlay();
}
export const hasTeam = () => teamChosen;

/** `?boot`: start the engine without a room (debugging). */
export function bootOnly(): void {
  const my = enterGame('', 'Starting');
  void boot('').then(() => isCurrent(my) && setPhase('playing'), (e) => backToLobby(`Engine failed to start: ${errorText(e)}`, true));
}

function leave(status = 'Not connected', bad = false): void {
  if (state.roomId) engine?.Cmd_ExecuteString('disconnect');
  transport.close();
  backToLobby(status, bad);
}
$('leave').onclick = () => leave();
$('cancel').onclick = () => leave('Cancelled');
transport.onclose = (reason) => {
  if (state.roomId) leave(`Disconnected: ${reason}`, true);
};
