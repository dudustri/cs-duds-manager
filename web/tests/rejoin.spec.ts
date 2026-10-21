// The host leaves and clicks Connect again at once, under the same name, while another player stays in.
import { expect, test } from '@playwright/test';
import { api, captureLog, joinFromList, profile, requireStack, uniqueName, waitForPlayers } from './fixtures.ts';

requireStack();

test('the host rejoins right after leaving while another player is in', async () => {
  const room = await api.createRoom('de_dust2', 'classic');
  const ctxA = await profile();
  const ctxB = await profile();
  try {
    const [a, b] = [await ctxA.newPage(), await ctxB.newPage()];
    const logA = captureLog(a);
    const [nameA, nameB] = [uniqueName('alice'), uniqueName('bob')];
    await a.goto('/');
    await a.evaluate(([id, t]) => localStorage.setItem('cs-host-tokens', JSON.stringify({ [id]: t })), [room.id, room.hostToken!]);
    await joinFromList(a, room.id, nameA);
    await joinFromList(b, room.id, nameB);
    await waitForPlayers(room, [nameA, nameB]);

    await a.getByTestId('leave').evaluate((el: HTMLElement) => el.click()); // behind the in-game overlay
    const mark = logA.length;
    await a.locator(`[data-room-id="${room.id}"]`).getByTestId('connect').click();
    await expect(a.locator('#status')).not.toContainText('already plays as');
    await expect.poll(() => logA.slice(mark).some((l) => l.includes('cs-duds: in game')), { timeout: 60_000 }).toBe(true);
    await waitForPlayers(room, [nameA, nameB], 30_000);
  } finally {
    await ctxA.close();
    await ctxB.close();
    await api.deleteRoom(room.id);
  }
});
