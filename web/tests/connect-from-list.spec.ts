// Phase 3 exit: a room created through the API shows up in the lobby list and Connect gets you in.
import { expect, test } from '@playwright/test';
import { api, joinFromList, profile, requireStack, uniqueName, waitForPlayers } from './fixtures.ts';

requireStack();

test('connect from the room list end to end', async () => {
  const room = await api.createRoom('de_dust2', 'classic');
  const ctx = await profile();
  try {
    const page = await ctx.newPage();
    await page.goto('/');
    const row = page.locator(`[data-testid=room-list] [data-room-id="${room.id}"]`);
    await expect(row).toBeVisible({ timeout: 15_000 });
    await expect(row).toContainText('de_dust2');
    await expect(row).toContainText('classic');
    await expect(row).toContainText('0/16');

    const name = uniqueName('carol');
    await joinFromList(page, room.id, name);
    await expect(page.getByTestId('progress')).toBeVisible({ timeout: 30_000 });
    await waitForPlayers(room, [name]);
    await expect(page.locator(`[data-room-id="${room.id}"]`)).toContainText('1/16', { timeout: 15_000 }); // within a poll
  } finally {
    await ctx.close();
    await api.deleteRoom(room.id);
  }
});
