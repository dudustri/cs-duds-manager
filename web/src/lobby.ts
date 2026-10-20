// The lobby: the server list and the create card.
import * as api from './api.ts';
import { backToLobby, enterGame, isCurrent, join, onChange, setStage, state } from './game.ts';
import { $, errorText, setNote } from './ui.ts';

let rooms: api.Room[] = [];
let catalogMaps: string[] = [];

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
  const busy = state.phase === 'loading';
  const key = JSON.stringify([rooms, busy, state.roomId]);
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
        if (state.phase !== 'lobby') return;
        void join(r.id, enterGame(r.map, 'Joining')); // enterGame is synchronous, so the fullscreen request counts as part of the click
      };
      tr.insertCell().append(b);
      return tr;
    }),
  );
}

export async function refreshRooms(): Promise<void> {
  try {
    rooms = await api.listRooms();
    setNote('list-note', '');
  } catch (e) {
    setNote('list-note', `Could not load rooms: ${errorText(e)}. Retrying.`, true);
  }
  renderRooms();
}

let lastPhase = state.phase;
onChange(() => {
  renderRooms();
  if (state.phase === 'lobby' && lastPhase !== 'lobby') void refreshRooms();
  lastPhase = state.phase;
});

// ---- Create card -----------------------------------------------------------------------------------------------

export async function loadCatalog(): Promise<void> {
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
// Room size: one click on a common size, or a custom number (the input's min/max make the form reject anything but 2-32).
const custom = $<HTMLInputElement>('max-custom');
$('max-players').replaceChildren(
  ...[4, 6, 8, 12, 16, 20, 32].map((n) => {
    const l = document.createElement('label');
    const i = document.createElement('input');
    i.type = 'radio';
    i.name = 'max-players';
    i.value = String(n);
    i.checked = n === 16;
    i.dataset.testid = 'max-players-option';
    i.onchange = () => (custom.value = '');
    const s = document.createElement('span');
    s.textContent = String(n);
    l.append(i, s);
    return l;
  }),
);
custom.oninput = () => $('max-players').querySelectorAll('input').forEach((i) => (i.checked = !custom.value && i.value === '16'));
const maxPlayers = () => Number(custom.value || $('max-players').querySelector<HTMLInputElement>('input:checked')?.value || 16);

$('create').onsubmit = async (ev) => {
  ev.preventDefault();
  if (state.phase !== 'lobby') return;
  const map = $<HTMLSelectElement>('map').value;
  const my = enterGame(map, 'Creating room'); // synchronous: see enterGame
  setStage('download', 'Creating room');
  try {
    const room = await api.createRoom(map, $<HTMLSelectElement>('mode').value, maxPlayers());
    api.hostTokens.set(room.id, room.hostToken);
    void refreshRooms();
    if (isCurrent(my)) await join(room.id, my);
  } catch (e) {
    if (!isCurrent(my)) return;
    backToLobby('', false);
    setNote('create-note', errorText(e), true);
  }
};
