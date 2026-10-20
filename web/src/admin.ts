// Admin page: sign in, list rooms, stop them.
import * as api from './api.ts';

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const note = (text: string, bad = false) => {
  $('note').textContent = text;
  $('note').classList.toggle('bad', bad);
};
const msg = (e: unknown): string => {
  if (e instanceof api.ApiError) {
    const known: Record<string, string> = {
      bad_password: 'Wrong password.',
      admin_disabled: 'Admin is not enabled on this server.',
      rate_limited: 'Too many attempts. Wait a few seconds and try again.',
    };
    return known[e.code] ?? e.message;
  }
  return e instanceof Error ? e.message : String(e);
};

let timer: ReturnType<typeof setInterval> | undefined;

function showLogin(): void {
  clearInterval(timer);
  $('rooms').hidden = true;
  $('login').hidden = false;
}

async function refresh(): Promise<void> {
  try {
    const rooms = await api.listRooms();
    $('rows').replaceChildren(
      ...rooms.map((r) => {
        const tr = document.createElement('tr');
        tr.dataset.roomId = r.id;
        for (const [t, c] of [[r.id, ''], [r.map, ''], [r.mode, ''], [`${r.players}/${r.maxPlayers}`, 'num']] as const) {
          const td = tr.insertCell();
          td.textContent = t;
          td.className = c;
        }
        const b = document.createElement('button');
        b.type = 'button';
        b.textContent = 'Stop';
        b.onclick = async () => {
          b.disabled = true;
          try {
            await api.deleteRoom(r.id);
            tr.remove();
            note(`Stopped room ${r.id}.`);
          } catch (e) {
            b.disabled = false;
            if (e instanceof api.ApiError && e.status === 403) return (note('Your session ended. Sign in again.', true), showLogin());
            note(msg(e), true);
          }
        };
        tr.insertCell().append(b);
        return tr;
      }),
    );
    if (!rooms.length) $('rows').innerHTML = '<tr><td colspan="5">No rooms are running.</td></tr>';
  } catch (e) {
    note(`Could not load rooms: ${msg(e)}`, true);
  }
}

function showRooms(): void {
  $('login').hidden = true;
  $('rooms').hidden = false;
  void refresh();
  timer = setInterval(() => void refresh(), 5000);
}

$('login-form').onsubmit = async (ev) => {
  ev.preventDefault();
  const pw = $<HTMLInputElement>('password');
  try {
    await api.adminLogin(pw.value);
    pw.value = '';
    note('');
    showRooms();
  } catch (e) {
    note(msg(e), true);
  }
};

void api.isAdmin().then((ok) => (ok ? showRooms() : showLogin()), () => (note('Could not reach the server.', true), showLogin()));
