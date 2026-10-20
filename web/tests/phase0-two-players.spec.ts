// Phase 0 exit: two browser windows play each other on de_dust2 through the relay.
import { expect, test } from '@playwright/test';
import { api, captureLog, joinFromList, profile, requireStack, uniqueName, waitForPlayers, xash } from './fixtures.ts';

requireStack();

test('two windows join one classic de_dust2 room and stay in for 60 s', async () => {
  const room = await api.createRoom('de_dust2', 'classic');
  const ctxA = await profile();
  const ctxB = await profile();
  try {
    const [a, b] = [await ctxA.newPage(), await ctxB.newPage()];
    const [logA, logB] = [captureLog(a), captureLog(b)];
    const [nameA, nameB] = [uniqueName('alice'), uniqueName('bob')];
    await joinFromList(a, room.id, nameA);
    await joinFromList(b, room.id, nameB);

    const players = await waitForPlayers(room, [nameA, nameB]);
    expect(new Set(players.map((p) => p.userid)).size).toBe(2);
    expect(new Set(players.map((p) => p.steamid)).size).toBe(2); // ReUnion + sv_lan 0: distinct ids per session address
    for (const p of players) expect(p.steamid).toMatch(/^STEAM_/);

    // each client's own `status` lists the other player (resent per attempt: a client still connecting drops it)
    await expect.poll(async () => (await xash(a, 'status'), logA.some((l) => l.includes(nameB))), { timeout: 30_000, intervals: [2000] }).toBe(true);
    await expect.poll(async () => (await xash(b, 'status'), logB.some((l) => l.includes(nameA))), { timeout: 30_000, intervals: [2000] }).toBe(true);

    // no SharedArrayBuffer / COOP / COEP needed
    expect(await a.evaluate(() => crossOriginIsolated)).toBe(false);

    for (const p of [a, b]) {
      await xash(p, 'jointeam 1');
      await xash(p, 'joinclass 1');
    }
    for (let i = 0; i < 12; i++) {
      await a.waitForTimeout(5000);
      expect((await api.players(room.id, room.hostToken!)).map((p) => p.name).sort()).toEqual([nameA, nameB].sort());
    }
  } finally {
    await ctxA.close();
    await ctxB.close();
    await api.deleteRoom(room.id);
  }
});
