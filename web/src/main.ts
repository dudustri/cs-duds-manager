// Two states: the lobby (server list + create card) and the game (full-viewport container with the canvas,
// a loading screen while joining, and an overlay while the mouse is free).
import type { Xash3D } from 'xash3d-fwgs';
import * as api from './api.ts';
import { loadBundles } from './assets.ts';
import { bootEngine, cleanName, setName } from './engine.ts';
import { CONNECT_CMD, RelayTransport } from './transport.ts';

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const lobby = $('lobby');
const game = $('game');
const canvas = $<HTMLCanvasElement>('canvas');
const loading = $('loading');
const overlay = $('overlay');
const nameInput = $<HTMLInputElement>('name');
const statusEl = $('status');
const controls = $('controls');

const transport = new RelayTransport();
let engine: Xash3D | undefined;
let booting: Promise<Xash3D> | undefined;
let roomId: string | undefined; // the room we are connected to (or joining)
let joined = false; // in the room and past the loading screen
let epoch = 0; // bumped on every enterGame/backToLobby so a late async step can tell it was cancelled
let wantLock = false; // the player clicked the canvas; the engine's own pointer-lock requests don't count
let busy = false; // between the click on Connect/Create and the end of joining
let catalogMaps: string[] = [];
let isAdmin = false;
let rooms: api.Room[] = [];

const errorText = (e: unknown): string => {
  if (e instanceof api.ApiError) {
    const known: Record<string, string> = {
      room_not_found: 'That room no longer exists.',
      too_many_sessions: 'Too many connections from your network. Try again in a minute.',
      rate_limited: 'Too many requests. Wait a moment and try again.',
      rooms_full: 'All room slots are taken. Join a room or wait for one to close.',
      start_failed: 'The room did not start in time. Try again.',
      vote_running: 'A vote is already running.',
      vote_too_soon: 'A vote just ended. Wait a few seconds before starting another.',
      unknown_map: 'The server does not know that map.',
    };
    return known[e.code] ?? e.message;
  }
  return e instanceof Error ? e.message : String(e);
};

function setStatus(text: string, bad = false): void {
  statusEl.textContent = text;
  statusEl.classList.toggle('bad', bad);
}
function setNote(id: string, text: string, bad = false): void {
  const el = $(id);
  el.textContent = text;
  el.classList.toggle('bad', bad);
}

// ---- Game state: show/hide, fullscreen, pointer lock, loading screen -------------------------------------------

const inGameView = () => !game.hidden;
const fullscreen = () => document.fullscreenElement === game;

function syncOverlay(): void {
  overlay.hidden = !(inGameView() && loading.hidden && !(wantLock && document.pointerLockElement));
  if (!overlay.hidden) void refreshControls();
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
function setStage(step: string, text: string, fraction?: number): void {
  $('load-stage').textContent = text;
  const at = STEPS.indexOf(step);
  [...$('steps').children].forEach((li, i) => {
    li.classList.toggle('done', i < at);
    li.classList.toggle('now', i === at);
  });
  $('bar').style.width = fraction === undefined ? '' : `${Math.round(fraction * 100)}%`;
  $('bar').classList.toggle('indeterminate', fraction === undefined);
}

/** Called synchronously from the click handler: the game takes over the screen at once. */
function enterGame(mapName: string, sub: string): number {
  busy = true;
  epoch++;
  lobby.hidden = true;
  game.hidden = false;
  game.classList.add('is-loading');
  loading.hidden = false;
  $('load-map').textContent = mapName;
  $('load-sub').textContent = sub;
  setStage('download', 'Preparing');
  requestFullscreen();
  syncOverlay();
  return epoch;
}

function loaded(): void {
  busy = false;
  joined = true;
  game.classList.remove('is-loading');
  loading.hidden = true;
  syncOverlay();
  renderRooms();
}

function backToLobby(status: string, bad: boolean): void {
  epoch++;
  busy = false;
  joined = false;
  roomId = undefined;
  game.hidden = true;
  game.classList.remove('is-loading');
  loading.hidden = true;
  overlay.hidden = true;
  lobby.hidden = false;
  if (document.fullscreenElement) void document.exitFullscreen().catch(() => {});
  if (document.pointerLockElement) document.exitPointerLock();
  setStatus(status, bad);
  renderRooms();
  void refreshRooms();
}

document.addEventListener('keydown', (ev) => {
  if (ev.key !== 'F11' || !inGameView()) return;
  ev.preventDefault();
  toggleFullscreen();
});
// The engine asks for the mouse by itself as soon as it is in game; only a click by the player may grant it,
// otherwise the overlay (and Leave) would be unreachable. Capture phase: runs before the engine's own handlers.
const grantLock = canvas.requestPointerLock.bind(canvas);
canvas.requestPointerLock = (...args) => (wantLock ? grantLock(...args) : Promise.resolve());
canvas.addEventListener('pointerdown', () => (wantLock = joined), true);
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

// ---- Engine ----------------------------------------------------------------------------------------------------

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
  if (m && roomId && joined) leave(`Disconnected: ${m[1].trim().slice(0, 100)}`, true);
}

const hostToken = () => (roomId ? api.hostTokens.get(roomId) : undefined);
const canManage = () => Boolean(hostToken()) || isAdmin;
/** Host token, else the session token (players), else the admin cookie. */
const auth = (): api.Auth => (hostToken() ? api.hostAuth(hostToken()) : transport.sessionToken ? api.sessionAuth(transport.sessionToken) : undefined);

async function join(id: string, my: number): Promise<void> {
  roomId = id;
  try {
    const x = await boot();
    if (epoch !== my) return;
    setStage('join', 'Joining server');
    setStatus('');
    setName(x, nameInput.value);
    await transport.open(id, api.hostAuth(hostToken()));
    if (epoch !== my) return transport.close();
    x.Cmd_ExecuteString(CONNECT_CMD);
    if (hostToken()) x.Cmd_ExecuteString('bind F3 amxmodmenu');
    setTimeout(() => epoch === my && loaded(), 2500); // shortcut: no "in game" signal from the engine; revisit if the map still loads when this fires
  } catch (e) {
    if (epoch !== my) return;
    transport.close();
    backToLobby(`Could not join: ${errorText(e)}`, true);
  }
}

function leave(status = 'Not connected', bad = false): void {
  if (roomId) engine?.Cmd_ExecuteString('disconnect');
  transport.close();
  backToLobby(status, bad);
}
$('leave').onclick = () => leave();
$('cancel').onclick = () => leave('Cancelled');
transport.onclose = (reason) => {
  if (roomId) leave(`Disconnected: ${reason}`, true);
};

// ---- Lobby: server list ----------------------------------------------------------------------------------------

function mapBadge(map: string): HTMLElement {
  const b = document.createElement('span');
  let h = 0;
  for (const c of map) h = (h * 31 + c.charCodeAt(0)) % 360;
  b.className = 'badge';
  b.style.setProperty('--h', String(h));
  b.textContent = map.replace(/^[a-z]{2,3}_/, '').slice(0, 2);
  return b;
}

let lastRooms = '';
function renderRooms(): void {
  $('room-count').textContent = `${rooms.length} ${rooms.length === 1 ? 'room' : 'rooms'} running`;
  const body = $('room-list');
  const key = JSON.stringify([rooms, busy, roomId]);
  if (key === lastRooms) return;
  lastRooms = key;
  if (!rooms.length) {
    body.innerHTML = '<tr class="empty"><td colspan="4">No rooms yet. Create one on the right and it starts in a few seconds.</td></tr>';
    return;
  }
  body.replaceChildren(
    ...rooms.map((r) => {
      const tr = document.createElement('tr');
      tr.dataset.roomId = r.id;
      const full = r.players >= r.maxPlayers;
      tr.classList.toggle('full', full);

      const map = tr.insertCell();
      map.className = 'map';
      const name = document.createElement('span');
      name.textContent = r.map;
      map.append(mapBadge(r.map), name);

      const mode = tr.insertCell();
      const chip = document.createElement('span');
      chip.className = 'chip';
      chip.textContent = r.mode;
      mode.append(chip);

      const pl = tr.insertCell();
      pl.className = 'pl';
      const count = document.createElement('span');
      count.textContent = `${r.players}/${r.maxPlayers}`;
      const fill = document.createElement('span');
      fill.className = 'fill';
      fill.innerHTML = '<i></i>';
      fill.firstElementChild!.setAttribute('style', `width:${Math.min(100, (r.players / r.maxPlayers) * 100)}%`);
      pl.append(count, fill);

      const b = document.createElement('button');
      b.type = 'button';
      b.dataset.testid = 'connect';
      b.className = 'primary';
      b.textContent = full ? 'Full' : 'Connect';
      b.disabled = busy || full;
      b.onclick = () => {
        if (busy) return;
        void join(r.id, enterGame(r.map, 'Joining')); // enterGame is synchronous, so the fullscreen request counts as part of the click
      };
      tr.insertCell().append(b);
      return tr;
    }),
  );
}
async function refreshRooms(): Promise<void> {
  try {
    rooms = await api.listRooms();
    setNote('list-note', '');
  } catch (e) {
    setNote('list-note', `Could not load rooms: ${errorText(e)}. Retrying.`, true);
  }
  renderRooms();
}

// ---- Lobby: create card ----------------------------------------------------------------------------------------

async function loadCatalog(): Promise<void> {
  if (catalogMaps.length) return;
  try {
    const c = await api.getCatalog();
    catalogMaps = c.maps;
    const fill = (id: string, items: string[], pick: string) => {
      const sel = $<HTMLSelectElement>(id);
      sel.replaceChildren(...items.map((v) => new Option(v, v)));
      if (items.includes(pick)) sel.value = pick;
    };
    fill('map', c.maps, 'de_dust2');
    fill('mode', c.modes, 'classic');
    $('vote-maps').replaceChildren(
      ...c.maps.map((m) => {
        const l = document.createElement('label');
        const i = document.createElement('input');
        i.type = 'checkbox';
        i.value = m;
        i.dataset.testid = 'vote-map-option';
        l.append(i, m);
        return l;
      }),
    );
  } catch (e) {
    setNote('create-note', `Could not load maps and modes: ${errorText(e)}`, true);
  }
}
$<HTMLSelectElement>('max-players').replaceChildren(...Array.from({ length: 31 }, (_, i) => new Option(`${i + 2} players`, String(i + 2))));
$<HTMLSelectElement>('max-players').value = '16';

$('create').onsubmit = async (ev) => {
  ev.preventDefault();
  if (busy) return;
  const map = $<HTMLSelectElement>('map').value;
  const my = enterGame(map, 'Creating room'); // synchronous: see enterGame
  setStage('download', 'Creating room');
  try {
    const room = await api.createRoom(map, $<HTMLSelectElement>('mode').value, Number($<HTMLSelectElement>('max-players').value));
    api.hostTokens.set(room.id, room.hostToken);
    void refreshRooms();
    if (epoch === my) await join(room.id, my);
  } catch (e) {
    if (epoch !== my) return;
    backToLobby('', false);
    setNote('create-note', errorText(e), true);
  }
};

// ---- Overlay: players, bots, votes -----------------------------------------------------------------------------

let players: api.Player[] = [];
async function refreshControls(): Promise<void> {
  const id = roomId;
  if (!id || !joined || overlay.hidden) return;
  const manage = canManage();
  controls.dataset.testid = manage ? 'host-panel' : 'player-panel'; // host-panel exists only in the host's browser
  $('controls-h').textContent = manage ? 'Host controls' : 'Players';
  $('bots').hidden = !manage;
  if (!manage) { // shortcut: GET /players is host/admin only, so players can't pick a Vote kick target yet
    $('players').innerHTML = '<li class="dim">Only the host sees the player list.</li>';
    return;
  }
  try {
    players = await api.listPlayers(id, auth());
    if (roomId !== id) return;
    setNote('host-note', '');
    const me = cleanName(nameInput.value);
    $('players').replaceChildren(
      ...players.map((p) => {
        const li = document.createElement('li');
        li.dataset.testid = 'player-row';
        li.innerHTML = '<span class="who"></span><span class="meta"></span>';
        li.querySelector('.who')!.textContent = p.name;
        li.querySelector('.meta')!.textContent = p.bot ? 'bot' : `${p.frags} frags, ${p.ping} ms`;
        const act = (testid: string, label: string, run: () => Promise<void>, done?: () => void) => {
          const b = document.createElement('button');
          b.type = 'button';
          b.dataset.testid = testid;
          b.textContent = label;
          b.onclick = async () => {
            b.disabled = true;
            try {
              await run();
              done?.();
            } catch (e) {
              b.disabled = false;
              setNote('host-note', errorText(e), true);
            }
          };
          li.append(b);
        };
        if (p.name !== me) {
          act('vote-kick', 'Vote kick', () => api.voteKick(id, p.userid, auth()), () => setNote('host-note', `Vote to kick ${p.name} started. Vote in game.`));
        }
        if (manage) act('kick', 'Kick', () => api.kick(id, p.userid, auth()), () => li.remove());
        return li;
      }),
    );
  } catch (e) {
    setNote('host-note', `Could not load players: ${errorText(e)}`, true);
  }
}
setInterval(() => void refreshControls(), 3000);

$('vote-start').onclick = async () => {
  if (!roomId) return;
  const maps = [...controls.querySelectorAll<HTMLInputElement>('[data-testid=vote-map-option]:checked')].map((i) => i.value);
  if (maps.length < 1 || maps.length > 4) return setNote('host-note', 'Pick between 1 and 4 maps.', true);
  try {
    await api.voteMap(roomId, maps, auth());
    setNote('host-note', 'Vote started. Players pick a map in game with the number keys.');
  } catch (e) {
    setNote('host-note', errorText(e), true);
  }
};

const bots = (run: () => Promise<void>) => async () => {
  if (!roomId) return;
  try {
    await run();
    setNote('host-note', '');
    void refreshControls();
  } catch (e) {
    setNote('host-note', errorText(e), true);
  }
};
const diff = () => Number($<HTMLSelectElement>('bot-diff').value);
$('bot-ct').onclick = bots(() => api.addBot(roomId!, 'ct', diff(), auth()));
$('bot-t').onclick = bots(() => api.addBot(roomId!, 't', diff(), auth()));
$('bot-clear').onclick = bots(() => api.removeBots(roomId!, auth()));

// ---- Start -----------------------------------------------------------------------------------------------------

const saved = (() => { try { return localStorage.getItem('cs-name'); } catch { return null; } })();
nameInput.value = saved ?? `player${Math.floor(100 + Math.random() * 900)}`;
nameInput.onchange = () => {
  nameInput.value = cleanName(nameInput.value);
  try { localStorage.setItem('cs-name', nameInput.value); } catch { /* not persisted */ }
};

void api.isAdmin().then((a) => (isAdmin = a), () => {});
void loadCatalog();
void refreshRooms();
setInterval(() => (void loadCatalog(), void refreshRooms()), 5000);
if (new URLSearchParams(location.search).has('boot')) {
  const my = enterGame('', 'Starting');
  void boot().then(() => epoch === my && loaded(), (e) => backToLobby(`Engine failed to start: ${errorText(e)}`, true));
}
