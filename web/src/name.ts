// The player name in the header. Names are unique: the server hands each one to a single browser (its token), so we
// only ever play under a name we hold. Typing checks the new name; Set (or Enter) claims it.
import * as api from './api.ts';
import { cleanName } from './engine.ts';
import { $, errorText } from './ui.ts';

const input = $<HTMLInputElement>('name');
const setButton = $<HTMLButtonElement>('name-set');
const load = (key: string) => { try { return localStorage.getItem(key) ?? ''; } catch { return ''; } };
const save = (key: string, value: string) => { try { localStorage.setItem(key, value); } catch { /* memory only */ } };

let mine = load('cs-name');
let token = load('cs-name-token');

/** The name we play under: one this browser holds on the server. */
export const myName = () => mine;

function hold(claimed: { name: string; token: string }, show = true): void {
  ({ name: mine, token } = claimed);
  save('cs-name', mine);
  save('cs-name-token', token);
  if (!show) return;
  input.value = mine;
  mark('');
}

/** '' while the box shows our own name (or a check is pending), 'ok' (green, Set shown) or 'taken'. */
function mark(state: '' | 'ok' | 'taken'): void {
  input.classList.toggle('ok', state === 'ok');
  input.classList.toggle('taken', state === 'taken');
  setButton.hidden = state !== 'ok';
}

let toastTimer = 0;
function toast(text: string, title = 'Name taken'): void {
  $('name-toast-title').textContent = title;
  $('name-toast-text').textContent = text;
  $('name-toast').hidden = false;
  clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => ($('name-toast').hidden = true), 6000);
}
$('name-toast-close').onclick = () => ($('name-toast').hidden = true);

let checkTimer = 0;
let checking = 0; // only the newest check may mark the box
input.oninput = () => {
  clearTimeout(checkTimer);
  mark('');
  const name = cleanName(input.value);
  if (!input.value.trim() || name === mine) return;
  const my = ++checking;
  checkTimer = window.setTimeout(async () => {
    const free = await api.nameAvailable(name, token).catch(() => undefined);
    if (my !== checking || free === undefined) return;
    mark(free ? 'ok' : 'taken');
    if (!free) toast(`"${name}" is already taken. Pick another name.`);
  }, 300);
};

async function claimTyped(): Promise<void> {
  checking++; // a check still in flight must not undo this
  await ready; // else the page-load renewal could land after this claim and take the old name back
  try {
    hold(await api.claimName(cleanName(input.value), token));
    $('name-toast').hidden = true;
  } catch (e) {
    const taken = e instanceof api.ApiError && e.code === 'name_taken';
    mark(taken ? 'taken' : 'ok'); // anything else (offline, rate limited): Set stays offered for another try
    toast(errorText(e), taken ? 'Name taken' : 'Name not set');
  }
}
setButton.onclick = () => void claimTyped();
input.onkeydown = (ev) => {
  if (ev.key === 'Enter' && !setButton.hidden) void claimTyped();
};

/** Before each join: a claim lapses after a day without a page load, and a tab can stay open longer than that. */
export const renewName = (): void => void api.claimName(mine, token).catch(() => {});

let ready = Promise.resolve();
/** Page load: renew our saved name, or claim a random one if we have none or someone took ours. */
export const initName = (): Promise<void> => (ready = renew());

async function renew(): Promise<void> {
  const shown = (input.value = mine);
  const untouched = () => input.value === shown; // don't overwrite what the player started typing meanwhile
  let lost = '';
  for (let i = 0; i < 5; i++) {
    const want = mine || `player${Math.floor(100 + Math.random() * 900)}`;
    try {
      hold(await api.claimName(want, token), untouched());
      if (lost) toast(`"${lost}" is taken by someone else now. You play as "${mine}" until you set another name.`);
      return;
    } catch (e) {
      if (!(e instanceof api.ApiError) || e.code !== 'name_taken') {
        mine ||= want; // server unreachable: play under it anyway, the check runs again on the next load
        if (untouched()) input.value = mine;
        return;
      }
      lost ||= mine;
      mine = '';
    }
  }
}
