// The game view: one phase (lobby -> loading -> playing -> lobby) that decides what is on screen, plus fullscreen,
// pointer lock, the engine and the room connection.
import type { Xash3D } from 'xash3d-fwgs';
import * as api from './api.ts';
import { loadBundles } from './assets.ts';
import { bootEngine, setName } from './engine.ts';
import { CONNECT_CMD, RelayTransport } from './transport.ts';
import { $, errorText, setNote } from './ui.ts';

export type Phase = 'lobby' | 'loading' | 'playing';

export const state = {
  phase: 'lobby' as Phase,
  roomId: undefined as string | undefined, // the room we are connected to (or joining)
  isAdmin: false,
};

const lobby = $('lobby');
const game = $('game');
const canvas = $<HTMLCanvasElement>('canvas');
const loading = $('loading');
const overlay = $('overlay');
const nameInput = $<HTMLInputElement>('name');

const transport = new RelayTransport();
let engine: Xash3D | undefined;
let booting: Promise<Xash3D> | undefined;
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

// Engine boot, once per page: bundles first, then the wasm engine.
function boot(): Promise<Xash3D> {
  booting ??= (async () => {
    const r = game.getBoundingClientRect(); // the engine sizes itself from the canvas attributes
    canvas.width = Math.max(320, Math.round(r.width));
    canvas.height = Math.max(240, Math.round(r.height));
    setStage('download', 'Downloading game files');
    const bundles = await loadBundles((d, t) =>
      setStage('download', `Downloading game files (${Math.round(d / 1e6)} / ${Math.round(t / 1e6)} MB)`, d / t),
    );
    setStage('engine', 'Starting engine');
    return (engine = await bootEngine(canvas, bundles, transport, onEngineLog));
  })();
  booting.catch(() => (booting = undefined));
  return booting;
}

// The relay keeps the channel open when the server drops us, so the engine's own log tells us. Only svc_disconnect
// means *we* were dropped; "Server disconnected, reconnecting" (changelevel) and "<name> was kicked" do not.
const DROPPED = /Server issued disconnect\. Reason: (.*)/;
function onEngineLog(line: string): void {
  const m = DROPPED.exec(line);
  if (m && state.phase === 'playing') leave(`Disconnected: ${m[1].trim().slice(0, 100)}`, true);
}

const hostToken = () => (state.roomId ? api.hostTokens.get(state.roomId) : undefined);
export const canManage = () => Boolean(hostToken()) || state.isAdmin;
/** Host token, else the session token (players), else the admin cookie. */
export const auth = (): api.Auth =>
  hostToken() ? api.hostAuth(hostToken()) : transport.sessionToken ? api.sessionAuth(transport.sessionToken) : undefined;

export async function join(id: string, my: number): Promise<void> {
  state.roomId = id;
  try {
    const x = await boot();
    if (!isCurrent(my)) return;
    setStage('join', 'Joining server');
    setNote('status', '');
    setName(x, nameInput.value);
    await transport.open(id, api.hostAuth(hostToken()));
    if (!isCurrent(my)) return transport.close();
    x.Cmd_ExecuteString(CONNECT_CMD);
    if (hostToken()) x.Cmd_ExecuteString('bind F3 amxmodmenu');
    setTimeout(() => isCurrent(my) && setPhase('playing'), 2500); // shortcut: no "in game" signal from the engine; revisit if the map still loads when this fires
  } catch (e) {
    if (!isCurrent(my)) return;
    transport.close();
    backToLobby(`Could not join: ${errorText(e)}`, true);
  }
}

/** `?boot`: start the engine without a room (debugging). */
export function bootOnly(): void {
  const my = enterGame('', 'Starting');
  void boot().then(() => isCurrent(my) && setPhase('playing'), (e) => backToLobby(`Engine failed to start: ${errorText(e)}`, true));
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
