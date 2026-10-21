// The lobby: servers (/, with a hero, filters and list or card view), players online (/#players) and the create
// page (/#create). Everything lives inside #lobby, which the game phase shows or hides.
import * as api from './api.ts';
import { backToLobby, enterGame, isCurrent, join, onChange, setStage, state } from './game.ts';
import { $, errorText, setNote } from './ui.ts';
import { cleanName } from './engine.ts';

const MODE_NAME: Record<string, string> = {
  classic: 'Classic', deathmatch: 'Deathmatch', gungame: 'GunGame', ffa: 'Free for all', knife: 'Knife arena',
  aim: 'Aim arena', awp: 'AWP', surf: 'Surf', kz: 'Climb', zombie: 'Zombie',
};
// The maps each mode is made for (name prefixes) and its Quick start map, when the catalog has them.
const MODE_MAPS: Record<string, { prefixes: string[]; quick: string }> = {
  classic: { prefixes: ['de_', 'cs_'], quick: 'de_dust2' },
  deathmatch: { prefixes: ['fy_', 'de_', 'awp_'], quick: 'fy_iceworld' },
  gungame: { prefixes: ['fy_', 'de_', 'cs_'], quick: 'de_dust2' },
  ffa: { prefixes: ['fy_', 'awp_', 'aim_'], quick: 'fy_pool_day' },
  knife: { prefixes: ['ka_', 'knife_', '35hp', '1hp'], quick: 'ka_knife' },
  aim: { prefixes: ['aim_', '3d_aim'], quick: 'aim_map' },
  awp: { prefixes: ['awp_'], quick: 'awp_map' },
  surf: { prefixes: ['surf_'], quick: 'surf_ski' },
  kz: { prefixes: ['kz_'], quick: 'kz_cliffez' },
  zombie: { prefixes: ['zm_', 'de_', 'cs_'], quick: 'de_dust2' },
};
const QUICK_PLAY = ['classic', 'zombie', 'gungame', 'awp', 'deathmatch', 'surf'];
const QUICK_TITLE: Record<string, string> = { classic: 'CS 1.6' };
const QUICK_SUB: Record<string, string> = {
  classic: 'Classic match', zombie: 'Humans vs zombies', gungame: 'Kill to level up', awp: 'Snipers only',
  deathmatch: 'Instant respawn', surf: 'Ride the ramps',
};
// small line icons for the quick play buttons (currentColor, 24x24)
const svg = (d: string) => `<svg viewBox="0 0 24 24" width="26" height="26" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round">${d}</svg>`;
const MODE_ICON: Record<string, string> = {
  classic: svg('<circle cx="12" cy="12" r="7"/><path d="M12 2v5M12 17v5M2 12h5M17 12h5"/>'),
  zombie: svg('<circle cx="12" cy="12" r="2"/><path d="M12 10V4M13.7 13l5.2 3M10.3 13l-5.2 3M12 4a8 8 0 0 1 6.9 4M5.1 8A8 8 0 0 1 12 4M18.9 16a8 8 0 0 1-13.8 0"/>'),
  gungame: svg('<path d="M6 14l6-6 6 6M6 20l6-6 6 6M6 8l6-6 6 6"/>'),
  awp: svg('<circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="3"/><path d="M12 3v6M12 15v6M3 12h6M15 12h6"/>'),
  deathmatch: svg('<path d="M4 20L17 7M7 7l13 13M15 5l4-1-1 4M9 5L5 4l1 4"/>'),
  surf: svg('<path d="M3 17c3 0 3-2 6-2s3 2 6 2 3-2 6-2M3 12c3 0 3-2 6-2s3 2 6 2 3-2 6-2"/>'),
};

let rooms: api.Room[] = [];
let people: api.OnlinePlayer[] = [];
let catalog: api.Catalog | undefined;
let selected: string | undefined; // the room shown in the details panel
let ping = 0; // ms from this browser to the site; every server runs on the same machine, so it is shared

const pref = (k: string, v?: string): string | null => {
  try {
    if (v !== undefined) localStorage.setItem(k, v);
    return localStorage.getItem(k);
  } catch {
    return null;
  }
};
let view = pref('cs-view') === 'cards' ? 'cards' : 'list';

/** A stable colour per map name, so a map looks the same everywhere. */
const hue = (map: string) => [...map].reduce((h, c) => (h * 31 + c.charCodeAt(0)) % 360, 0);
const shortName = (map: string) => map.replace(/^[a-z0-9]{2,3}_/, '');
const modeName = (m: string) => MODE_NAME[m] ?? m;
/** The maps a mode is played on (the server decides: special modes only their own maps, the rest everything else). */
const mapsFor = (mode: string) => catalog?.modeMaps?.[mode] ?? catalog?.maps ?? [];
const quickMap = (mode: string) => {
  const want = MODE_MAPS[mode]?.quick;
  const maps = mapsFor(mode);
  return want && maps.includes(want) ? want : (maps.find((m) => MODE_MAPS[mode]?.prefixes.some((p) => m.startsWith(p))) ?? 'de_dust2');
};

/** The map's screenshot (web/img/maps), or a coloured tile with its initials when it has none. */
function mapThumb(map: string, cls: string): HTMLElement {
  const box = document.createElement('span');
  box.className = cls;
  box.style.setProperty('--h', String(hue(map)));
  box.dataset.initials = shortName(map).slice(0, 2);
  const img = document.createElement('img');
  img.alt = '';
  img.loading = 'lazy';
  img.src = `/img/maps/${map}.webp`;
  img.onerror = () => img.remove();
  box.append(img);
  return box;
}

/** Whether the server lists a player in that room under the name typed in the name box. */
const nameTakenIn = (room: string | undefined) => {
  const me = cleanName($<HTMLInputElement>('name').value).toLowerCase();
  return people.some((p) => p.room === room && p.name.toLowerCase() === me);
};
const joinRoom = (r: { id: string; map: string }) => {
  if (state.phase !== 'lobby') return;
  // names are unique per server: the game would quietly rename a second "Bob" to "(1)Bob". The server's list lags a
  // few seconds, so right after we leave a room the name it still lists there is our own.
  if (r.id !== state.leftRoomId && nameTakenIn(r.id)) {
    setNote('status', `Someone in that server already plays as "${cleanName($<HTMLInputElement>('name').value)}". Pick another name first.`, true);
    $('name').focus();
    return;
  }
  setNote('status', '');
  hideToast();
  void join(r.id, r.map, enterGame(r.map, 'Joining')); // enterGame is synchronous, so the fullscreen request counts as part of the click
};
const isMine = (r: api.Room) => Boolean(api.hostTokens.get(r.id));
const isFull = (r: api.Room) => r.players >= r.maxPlayers;
const uptime = (r: api.Room) => {
  const mins = Math.max(0, Math.round((Date.now() - Date.parse(r.createdAt)) / 60_000));
  return mins < 1 ? 'just started' : mins < 60 ? `${mins} min` : `${Math.floor(mins / 60)} h ${mins % 60} min`;
};

function select(id: string | undefined): void {
  selected = id;
  renderRooms();
}
// the details float over the right edge (a modal on small screens): close with ×, the backdrop or Esc
$('det-x').onclick = () => select(undefined);
$('details-backdrop').onclick = () => select(undefined);
document.addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape' && selected && state.phase === 'lobby') select(undefined);
});

function renderDetails(): void {
  const r = rooms.find((x) => x.id === selected); // an invite link may name a room the list hasn't loaded yet
  $('details').hidden = !r;
  $('details-backdrop').hidden = !r;
  if (!r) return;
  const art = $('det-art');
  art.style.setProperty('--h', String(hue(r.map)));
  art.dataset.name = shortName(r.map); // shown when the map has no screenshot (CSS ::after)
  const shot = $<HTMLImageElement>('det-shot');
  if (shot.dataset.map !== r.map) {
    shot.dataset.map = r.map;
    shot.hidden = false;
    shot.onerror = () => (shot.hidden = true);
    shot.src = `/img/maps/${r.map}.webp`;
  }
  $('det-map').textContent = r.map;
  $('det-count').textContent = `${r.players} / ${r.maxPlayers}`;
  $('det-of').textContent = r.players === 1 ? 'player' : 'players';
  $('det-fill').style.width = `${Math.min(100, (r.players / r.maxPlayers) * 100)}%`;
  $('det-mode').textContent = modeName(r.mode);
  $('det-mode').dataset.mode = r.mode;
  $('det-host').textContent = r.host || 'unknown';
  $('det-up').textContent = uptime(r);
  $('det-settings').replaceChildren(
    ...(r.settings ?? []).flatMap((s) => {
      const dt = document.createElement('dt');
      dt.textContent = s.label;
      const dd = document.createElement('dd');
      dd.textContent = s.value;
      return [dt, dd];
    }),
  );
  const b = $<HTMLButtonElement>('det-join');
  b.textContent = isFull(r) ? 'Server is full' : 'Join server';
  b.disabled = isFull(r) || state.phase !== 'lobby';
  b.onclick = () => joinRoom(r);
  const stop = $<HTMLButtonElement>('det-stop');
  stop.hidden = !isMine(r);
  stop.onclick = async () => {
    if (!confirm(`Close the ${r.map} server? Everyone in it will be disconnected.`)) return;
    stop.disabled = true;
    try {
      await api.stopRoom(r.id, api.hostAuth(api.hostTokens.get(r.id)));
      selected = undefined;
      await refreshRooms();
    } catch (e) {
      setNote('status', `Could not close the server: ${errorText(e)}`, true);
    } finally {
      stop.disabled = false;
    }
  };
}

// ---- Routes ----------------------------------------------------------------------------------------------------

function route(): void {
  const h = location.hash;
  const page = h === '#create' ? 'create' : h === '#players' ? 'players' : 'servers';
  const invite = /^#room\/([a-z0-9]+)$/.exec(h);
  if (invite) select(invite[1]);
  $('view-list').hidden = page !== 'servers';
  $('hero').hidden = page !== 'servers';
  $('view-players').hidden = page !== 'players';
  $('view-create').hidden = page !== 'create';
  for (const a of document.querySelectorAll<HTMLAnchorElement>('.nav a')) {
    if (a.dataset.route === page) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  if (page === 'create') $('view-create').querySelector<HTMLInputElement>('input[name=mode]:checked')?.focus();
}
window.addEventListener('hashchange', route);

// ---- Server list: filters, sort, list or cards -----------------------------------------------------------------

function shown(): api.Room[] {
  const q = $<HTMLInputElement>('list-search').value.trim().toLowerCase();
  const mode = $<HTMLSelectElement>('list-mode').value;
  const sort = $<HTMLSelectElement>('list-sort').value;
  const by: Record<string, (a: api.Room, b: api.Room) => number> = {
    players: (a, b) => b.players - a.players,
    map: (a, b) => a.map.localeCompare(b.map),
    mode: (a, b) => modeName(a.mode).localeCompare(modeName(b.mode)),
    ping: () => 0, // shared by every server (one machine)
    new: (a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt),
  };
  return rooms
    .filter((r) => r.map.includes(q) && (!mode || r.mode === mode))
    .sort((a, b) => Number(isMine(b)) - Number(isMine(a)) || (by[sort] ?? by.players)(a, b) || a.map.localeCompare(b.map));
}

function joinButton(r: api.Room, busy: boolean): HTMLButtonElement {
  const b = document.createElement('button');
  b.type = 'button';
  b.dataset.testid = 'connect';
  b.className = 'primary';
  b.textContent = isFull(r) ? 'Full' : 'Join';
  b.disabled = busy || isFull(r);
  b.onclick = (ev) => (ev.stopPropagation(), joinRoom(r));
  return b;
}

function playersCell(r: api.Room): HTMLElement {
  const pl = document.createElement('div');
  pl.className = 'pl';
  const count = document.createElement('span');
  count.textContent = `${r.players}/${r.maxPlayers}`;
  const fill = document.createElement('span');
  fill.className = 'fill';
  const i = document.createElement('i');
  i.style.width = `${Math.min(100, (r.players / r.maxPlayers) * 100)}%`;
  fill.append(i);
  pl.append(count, fill);
  return pl;
}

function mineTag(): HTMLElement {
  const tag = document.createElement('span');
  tag.className = 'tag';
  tag.textContent = 'Your server';
  return tag;
}

function selectable(el: HTMLElement, r: api.Room): void {
  el.dataset.roomId = r.id;
  el.tabIndex = 0;
  el.classList.toggle('full', isFull(r));
  el.classList.toggle('mine', isMine(r));
  el.classList.toggle('selected', r.id === selected);
  el.setAttribute('aria-selected', String(r.id === selected));
  el.onclick = () => select(r.id);
  el.onkeydown = (ev) => {
    if (ev.target === el && (ev.key === 'Enter' || ev.key === ' ')) (ev.preventDefault(), select(r.id));
  };
}

let lastRooms = '';
function renderRooms(): void {
  $('room-count').textContent = `${rooms.length} ${rooms.length === 1 ? 'server' : 'servers'}`;
  const busy = state.phase === 'loading';
  const list = shown();
  const key = JSON.stringify([list, busy, state.roomId, selected, view, ping, Math.floor(Date.now() / 60_000)]);
  if (key === lastRooms) return;
  lastRooms = key;
  renderDetails();
  $('no-rooms').hidden = rooms.length > 0;
  $('no-match').hidden = !rooms.length || list.length > 0;
  const table = $('room-list').closest('table')!;
  table.hidden = view !== 'list' || !list.length;
  $('room-cards').hidden = view !== 'cards' || !list.length;
  for (const b of document.querySelectorAll<HTMLButtonElement>('.view-toggle button')) b.setAttribute('aria-pressed', String(b.dataset.view === view));

  if (view === 'list') {
    $('room-list').replaceChildren(
      ...list.map((r) => {
        const tr = document.createElement('tr');
        selectable(tr, r);
        const map = document.createElement('div'); // flex on the <td> itself would break the table's row lines
        map.className = 'map';
        const name = document.createElement('span');
        name.textContent = r.map;
        map.append(mapThumb(r.map, 'thumb'), name);
        if (isMine(r)) map.append(mineTag());
        tr.insertCell().append(map);
        const chip = document.createElement('span');
        chip.className = 'chip mode-chip';
        chip.dataset.mode = r.mode;
        chip.textContent = modeName(r.mode);
        tr.insertCell().append(chip);
        const host = tr.insertCell();
        host.className = 'host';
        host.textContent = r.host || '—';
        tr.insertCell().append(playersCell(r));
        const p = tr.insertCell();
        p.className = 'dim nowrap';
        p.textContent = ping ? `${ping} ms` : '—';
        const up = tr.insertCell();
        up.className = 'dim nowrap';
        up.textContent = uptime(r);
        tr.insertCell().append(joinButton(r, busy));
        return tr;
      }),
    );
  } else {
    $('room-cards').replaceChildren(
      ...list.map((r) => {
        const card = document.createElement('article');
        card.className = 'room-card';
        selectable(card, r);
        const body = document.createElement('div');
        body.className = 'rc-body';
        const title = document.createElement('h3');
        title.textContent = r.map;
        const meta = document.createElement('p');
        meta.className = 'rc-meta';
        const chip = document.createElement('span');
        chip.className = 'chip mode-chip';
        chip.dataset.mode = r.mode;
        chip.textContent = modeName(r.mode);
        const host = document.createElement('span');
        host.className = 'dim';
        host.textContent = r.host ? `by ${r.host}` : '';
        meta.append(chip, host);
        const foot = document.createElement('div');
        foot.className = 'rc-foot';
        foot.append(playersCell(r), joinButton(r, busy));
        body.append(title, meta, foot);
        const art = mapThumb(r.map, 'rc-art');
        if (isMine(r)) art.append(mineTag());
        card.append(art, body);
        return card;
      }),
    );
  }
}
for (const id of ['list-search', 'list-mode', 'list-sort']) $(id).addEventListener('input', renderRooms);
for (const b of document.querySelectorAll<HTMLButtonElement>('.view-toggle button')) {
  b.onclick = () => ((view = b.dataset.view === 'cards' ? 'cards' : 'list'), pref('cs-view', view), renderRooms());
}

// ---- Hero: live numbers and quick play -------------------------------------------------------------------------

function renderHero(): void {
  $('stat-players').textContent = String(rooms.reduce((n, r) => n + r.players, 0));
  $('stat-servers').textContent = String(rooms.length);
  $('stat-maps').textContent = String(new Set(rooms.map((r) => r.map)).size);
  for (const b of $('quick-modes').querySelectorAll<HTMLButtonElement>('button')) {
    const n = rooms.filter((r) => r.mode === b.dataset.mode).reduce((s, r) => s + r.players, 0);
    b.title = n ? `${n} playing ${modeName(b.dataset.mode!)} now` : `Start a ${modeName(b.dataset.mode!)} game`;
  }
}

function renderQuickModes(modes: string[]): void {
  $('quick-modes').replaceChildren(
    ...QUICK_PLAY.filter((m) => modes.includes(m)).map((m) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'quick-mode';
      b.dataset.mode = m;
      b.dataset.testid = 'quick-play';
      b.innerHTML = MODE_ICON[m] ?? MODE_ICON.classic;
      const text = document.createElement('span');
      text.className = 'qm-text';
      const name = document.createElement('strong');
      name.textContent = QUICK_TITLE[m] ?? modeName(m);
      const sub = document.createElement('small');
      sub.textContent = QUICK_SUB[m] ?? '';
      text.append(name, sub);
      b.append(text);
      b.onclick = () => quickPlay(m);
      return b;
    }),
  );
  renderHero();
}

/** Joins the fullest server of that mode with a free slot, or starts one with standard settings and joins it. */
function quickPlay(mode: string): void {
  if (state.phase !== 'lobby') return;
  const open = rooms.filter((r) => r.mode === mode && !isFull(r)).sort((a, b) => b.players - a.players)[0];
  if (open) return joinRoom(open);
  const map = quickMap(mode);
  const my = enterGame(map, 'Starting server'); // synchronous, inside the click: see enterGame
  setStage('download', 'Starting server');
  void (async () => {
    try {
      const room = await api.createRoom(map, mode, 16, {}, $<HTMLInputElement>('name').value);
      api.hostTokens.set(room.id, room.hostToken);
      if (isCurrent(my)) await join(room.id, map, my);
    } catch (e) {
      if (isCurrent(my)) backToLobby(`Could not start a ${modeName(mode)} server: ${errorText(e)}`, true);
    }
  })();
}

// ---- Players online ----------------------------------------------------------------------------------------------

function renderPeople(): void {
  $('no-people').hidden = people.length > 0;
  $('people').closest('table')!.hidden = !people.length;
  $('people').replaceChildren(
    ...[...people].sort((a, b) => b.frags - a.frags || a.name.localeCompare(b.name)).map((p) => {
      const tr = document.createElement('tr');
      tr.insertCell().textContent = p.name;
      const f = tr.insertCell();
      f.className = 'nowrap';
      f.textContent = String(p.frags);
      const where = tr.insertCell();
      const map = document.createElement('div');
      map.className = 'map';
      const name = document.createElement('span');
      name.textContent = `${p.map} · ${modeName(p.mode)}`;
      map.append(mapThumb(p.map, 'thumb'), name);
      where.append(map);
      const r = rooms.find((x) => x.id === p.room);
      const cell = tr.insertCell();
      if (r) cell.append(joinButton(r, state.phase === 'loading'));
      return tr;
    }),
  );
}

export async function refreshRooms(): Promise<void> {
  try {
    const t0 = performance.now();
    rooms = await api.listRooms();
    ping = Math.round(performance.now() - t0);
    people = await api.onlinePlayers().catch(() => people);
    if (!nameTakenIn(state.leftRoomId)) state.leftRoomId = undefined; // the server caught up
    setNote('list-note', '');
  } catch (e) {
    setNote('list-note', `Could not load servers: ${errorText(e)}. Retrying.`, true);
  }
  renderRooms();
  renderHero();
  renderPeople();
}

// the in-game map vote offers only maps of the room's mode
function filterVoteMaps(): void {
  const r = rooms.find((x) => x.id === state.roomId);
  const ok = new Set(r ? mapsFor(r.mode) : []);
  for (const l of $('vote-maps').querySelectorAll<HTMLElement>('label')) l.classList.toggle('off-mode', !ok.has(l.dataset.map!));
}

let lastPhase = state.phase;
onChange(() => {
  filterVoteMaps();
  renderRooms();
  if (state.phase === 'lobby' && lastPhase !== 'lobby') void refreshRooms();
  lastPhase = state.phase;
});

// ---- Create card -----------------------------------------------------------------------------------------------

const MODE_TEXT: Record<string, string> = {
  classic: 'Bomb and hostage rounds. Dead players watch until the next round.',
  deathmatch: 'Respawn right away; the round never ends.',
  gungame: 'Every kill moves you to the next weapon. First through the knife wins.',
  ffa: 'Everyone against everyone, instant respawn, buy anywhere.',
  knife: 'Knives only. Nothing to buy, nothing on the floor.',
  aim: 'An AK or M4 plus a Deagle and full armour for everyone. Short rounds.',
  awp: 'Snipers only: an AWP and a Deagle for everyone, nothing to buy.',
  surf: 'Slide the ramps: high air control, no fall damage, instant respawn.',
  kz: 'Climb maps with a timer from the start button to the end. No fighting.',
  zombie: 'One player turns into a zombie; every hit infects. Survive the round.',
};
const chosenMode = () => $('mode-cards').querySelector<HTMLInputElement>('input:checked')?.value ?? 'classic';

/** One card per mode: a radio with the mode's name and what it plays like. */
const MODE_ORDER = ['classic', 'deathmatch', 'gungame', 'ffa', 'zombie', 'aim', 'awp', 'knife', 'surf', 'kz'];
function renderModes(modes: string[]): void {
  const rank = (m: string) => (MODE_ORDER.includes(m) ? MODE_ORDER.indexOf(m) : MODE_ORDER.length);
  $('mode-cards').replaceChildren(
    ...[...modes].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b)).map((m) => {
      const l = document.createElement('label');
      l.className = 'mode-card';
      l.dataset.testid = 'mode-option';
      l.dataset.mode = m;
      const i = document.createElement('input');
      i.type = 'radio';
      i.name = 'mode';
      i.value = m;
      i.checked = m === 'classic' || (m === modes[0] && !modes.includes('classic'));
      i.onchange = () => (renderOptions(), filterMaps());
      const t = document.createElement('span');
      t.className = 'mode-name';
      t.textContent = MODE_NAME[m] ?? m;
      const d = document.createElement('small');
      d.textContent = MODE_TEXT[m] ?? '';
      const quick = document.createElement('button');
      quick.type = 'button';
      quick.className = 'quick';
      quick.dataset.testid = 'quick-start';
      quick.textContent = 'Quick start';
      quick.title = `${MODE_NAME[m] ?? m} on ${quickMap(m)}, 16 players, standard settings`;
      quick.onclick = (ev) => (ev.preventDefault(), void createServer(quickMap(m), m, 16, {}, quick)); // {}: every option at its default
      l.append(i, t, d, quick);
      return l;
    }),
  );
}

/** The selected mode's options, as segmented controls (one radio group per option). */
function renderOptions(): void {
  const mode = chosenMode();
  $('mode-options').replaceChildren(
    ...(catalog?.options?.[mode] ?? []).map((o) => {
      const row = document.createElement('div');
      row.className = 'opt';
      row.setAttribute('role', 'radiogroup');
      const name = document.createElement('span');
      name.textContent = o.label;
      name.id = `opt-${o.cvar}`;
      row.setAttribute('aria-labelledby', name.id);
      const seg = document.createElement('div');
      seg.className = 'seg';
      for (const [value, label] of o.values) {
        const l = document.createElement('label');
        const i = document.createElement('input');
        i.type = 'radio';
        i.name = o.cvar;
        i.value = value;
        i.checked = value === o.default;
        const s = document.createElement('span');
        s.textContent = label;
        l.append(i, s);
        seg.append(l);
      }
      row.append(name, seg);
      return row;
    }),
  );
}
const chosenOptions = (): Record<string, string> =>
  Object.fromEntries([...$('mode-options').querySelectorAll<HTMLInputElement>('input:checked')].map((i) => [i.name, i.value]));

/** The create page's map list, narrowed by the search box; keeps the chosen map while it still matches. */
function filterMaps(): void {
  const sel = $<HTMLSelectElement>('map');
  const keep = sel.value || 'de_dust2';
  const q = $<HTMLInputElement>('map-search').value.trim().toLowerCase();
  // the chosen mode's own maps first, then the rest, each alphabetically
  const fits = (m: string) => (MODE_MAPS[chosenMode()]?.prefixes ?? []).some((p) => m.startsWith(p));
  const all = mapsFor(chosenMode());
  const maps = all.filter((m) => m.includes(q)).sort((a, b) => Number(fits(b)) - Number(fits(a)) || a.localeCompare(b));
  sel.replaceChildren(...maps.map((v) => new Option(v, v)));
  sel.value = maps.includes(keep) && (fits(keep) || !maps.some(fits)) ? keep : (maps[0] ?? '');
  $('map-count').textContent = q ? `${maps.length} of ${all.length} maps` : `${maps.length} maps for ${modeName(chosenMode())}`;
}
$('map-search').oninput = filterMaps;

export async function loadCatalog(): Promise<void> {
  if (catalog) return;
  try {
    const c = await api.getCatalog();
    catalog = c;
    filterMaps();
    renderModes(c.modes);
    renderOptions();
    renderQuickModes(c.modes);
    $('list-mode').append(...c.modes.map((m) => new Option(modeName(m), m)));
    $('vote-maps').replaceChildren(
      ...c.maps.map((m) => {
        const l = document.createElement('label');
        const i = document.createElement('input');
        i.type = 'checkbox';
        i.value = m;
        i.dataset.testid = 'vote-map-option';
        l.dataset.map = m;
        l.append(i, m);
        return l;
      }),
    );
    $<HTMLInputElement>('vote-search').oninput = () => {
      const q = $<HTMLInputElement>('vote-search').value.trim().toLowerCase();
      for (const l of $('vote-maps').querySelectorAll<HTMLElement>('label')) l.hidden = !l.dataset.map!.includes(q);
    };
  } catch (e) {
    setNote('create-note', `Could not load maps and modes: ${errorText(e)}`, true);
  }
}
// Room size: a stepper (type, or - / +) with one-click common sizes below; the input's min/max reject anything else.
const size = $<HTMLInputElement>('max-custom');
const clampSize = (n: number) => Math.min(32, Math.max(2, Math.round(n) || 16));
const syncPresets = () => $('max-players').querySelectorAll('input').forEach((i) => (i.checked = i.value === size.value));
$('max-players').replaceChildren(
  ...[4, 6, 8, 12, 16, 20, 32].map((n) => {
    const l = document.createElement('label');
    const i = document.createElement('input');
    i.type = 'radio';
    i.name = 'max-players';
    i.value = String(n);
    i.dataset.testid = 'max-players-option';
    i.onchange = () => (size.value = String(n));
    const s = document.createElement('span');
    s.textContent = String(n);
    l.append(i, s);
    return l;
  }),
);
const step = (d: number) => ((size.value = String(clampSize(Number(size.value) + d))), syncPresets());
$('size-down').onclick = () => step(-1);
$('size-up').onclick = () => step(1);
size.oninput = syncPresets;
size.onblur = () => size.value && ((size.value = String(clampSize(Number(size.value)))), syncPresets());
syncPresets();
const maxPlayers = () => clampSize(Number(size.value));

// Creating starts the server and shows it in the list; the creator joins from the toast (or later, from the list).
async function createServer(map: string, mode: string, size: number, options: Record<string, string>, button: HTMLButtonElement): Promise<void> {
  if (button.disabled) return;
  const label = button.textContent;
  button.disabled = true;
  button.textContent = 'Starting server…';
  setNote('create-note', '');
  try {
    const room = await api.createRoom(map, mode, size, options, $<HTMLInputElement>('name').value);
    api.hostTokens.set(room.id, room.hostToken);
    history.pushState(null, '', `#room/${room.id}`);
    route();
    await refreshRooms();
    showToast(room, `${MODE_NAME[mode] ?? mode} on ${map}, ${size} players. It's first in your server list.`);
  } catch (e) {
    setNote('create-note', errorText(e), true);
  } finally {
    button.disabled = false;
    button.textContent = label;
  }
}

// ---- Toast: "server created", with Join and an invite link ----------------------------------------------------

let toastTimer = 0;
function showToast(room: api.CreatedRoom, text: string): void {
  $('toast-text').textContent = text;
  $<HTMLInputElement>('toast-url').value = `${location.origin}/#room/${room.id}`;
  $('toast-join').onclick = () => joinRoom(room);
  $('toast').hidden = false;
  clearTimeout(toastTimer);
  toastTimer = window.setTimeout(hideToast, 60_000);
}
function hideToast(): void {
  $('toast').hidden = true;
}
$('toast-close').onclick = hideToast;
$('toast-copy').onclick = async () => {
  const url = $<HTMLInputElement>('toast-url');
  try {
    await navigator.clipboard.writeText(url.value);
    $('toast-copy').textContent = 'Copied';
  } catch {
    url.select(); // no clipboard permission: leave it selected for Ctrl+C
  }
  setTimeout(() => ($('toast-copy').textContent = 'Copy'), 2000);
};

$('create').onsubmit = (ev) => {
  ev.preventDefault();
  void createServer($<HTMLSelectElement>('map').value, chosenMode(), maxPlayers(), chosenOptions(), $<HTMLButtonElement>('create-room'));
};

route();
