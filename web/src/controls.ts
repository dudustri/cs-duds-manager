// The in-game overlay panel: players (with kick / vote kick), bots and map votes.
import * as api from './api.ts';
import { cleanName } from './engine.ts';
import { auth, canManage, hasTeam, onChange, overlayOpen, pickTeam, state } from './game.ts';
import { $, errorText, setNote } from './ui.ts';

const controls = $('controls');
const nameInput = $<HTMLInputElement>('name');

async function refreshControls(): Promise<void> {
  const id = state.roomId;
  if (!id || state.phase !== 'playing' || !overlayOpen()) return;
  const manage = canManage();
  controls.dataset.testid = manage ? 'host-panel' : 'player-panel'; // host-panel exists only in the host's browser
  $('controls-h').textContent = manage ? 'Host controls' : 'Players';
  $('bots').hidden = !manage;
  try {
    const players = await api.listPlayers(id, auth());
    if (state.roomId !== id) return;
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
onChange(() => {
  $('teams').classList.toggle('picked', hasTeam());
  $('teams-h').textContent = hasTeam() ? 'Change team' : 'Choose your team';
  void refreshControls();
});
for (const b of $('teams').querySelectorAll<HTMLButtonElement>('[data-team]')) b.onclick = () => pickTeam(Number(b.dataset.team));
setInterval(() => void refreshControls(), 3000);

$('vote-start').onclick = async () => {
  if (!state.roomId) return;
  const maps = [...controls.querySelectorAll<HTMLInputElement>('[data-testid=vote-map-option]:checked')].map((i) => i.value);
  if (maps.length < 1 || maps.length > 4) return setNote('host-note', 'Pick between 1 and 4 maps.', true);
  try {
    await api.voteMap(state.roomId, maps, auth());
    setNote('host-note', 'Vote started. Players pick a map in game with the number keys.');
  } catch (e) {
    setNote('host-note', errorText(e), true);
  }
};

const bots = (run: (id: string) => Promise<void>) => async () => {
  if (!state.roomId) return;
  try {
    await run(state.roomId);
    setNote('host-note', '');
    void refreshControls();
  } catch (e) {
    setNote('host-note', errorText(e), true);
  }
};
const diff = () => Number($<HTMLSelectElement>('bot-diff').value);
$('bot-ct').onclick = bots((id) => api.addBot(id, 'ct', diff(), auth()));
$('bot-t').onclick = bots((id) => api.addBot(id, 't', diff(), auth()));
$('bot-clear').onclick = bots((id) => api.removeBots(id, auth()));
