// Amendment A: room size on create, F11 fullscreen of the game, pointer lock on click, overlay while unlocked.
// Headless Chromium can't really grab the mouse or the screen, so the browser APIs are stubbed and we assert
// the page called them on the right element and reacts to the browser's lock events.
import { expect, test } from '@playwright/test';
import { api, profile, requireStack, uniqueName } from './fixtures.ts';

requireStack();

test('create form sets the room size', async () => {
  const ctx = await profile({ width: 1024, height: 768 });
  let id = '';
  try {
    const page = await ctx.newPage();
    await page.goto('/');
    await page.getByTestId('name-input').fill(uniqueName('size'));
    await page.getByTestId('map-select').selectOption('de_dust2');
    await page.getByTestId('mode-select').selectOption('classic');
    await page.getByTestId('max-players-custom').fill('10'); // custom size; the chips are 4/6/8/12/16/20/32
    const created = page.waitForResponse((r) => r.url().endsWith('/api/rooms') && r.request().method() === 'POST');
    await page.getByTestId('create-room').click();
    const body = await (await created).json();
    id = body.id;
    expect(body.maxPlayers).toBe(10);
    await expect(page.locator(`[data-testid=room-list] [data-room-id="${id}"]`)).toContainText('/10', { timeout: 15_000 });
  } finally {
    await ctx.close();
    if (id) await api.deleteRoom(id);
  }
});

test('F11 fullscreens the game, click locks the mouse, Esc shows the overlay', async () => {
  const room = await api.createRoom('de_dust2', 'classic');
  const ctx = await profile({ width: 1024, height: 768 });
  try {
    const page = await ctx.newPage();
    await page.addInitScript(() => {
      const w = window as any;
      w.__calls = [] as string[];
      Element.prototype.requestFullscreen = function () {
        w.__calls.push('fullscreen:' + (this as Element).getAttribute('data-testid'));
        return Promise.resolve();
      };
      Element.prototype.requestPointerLock = function () {
        w.__calls.push('pointerlock:' + (this as Element).tagName.toLowerCase());
        Object.defineProperty(document, 'pointerLockElement', { configurable: true, get: () => this });
        document.dispatchEvent(new Event('pointerlockchange'));
        return Promise.resolve() as any;
      };
      w.__unlock = () => {
        Object.defineProperty(document, 'pointerLockElement', { configurable: true, get: () => null });
        document.dispatchEvent(new Event('pointerlockchange'));
      };
    });
    await page.goto('/');
    await page.getByTestId('name-input').fill(uniqueName('ui'));
    await page.locator(`[data-testid=room-list] [data-room-id="${room.id}"] [data-testid=connect]`).click();
    const game = page.getByTestId('game');
    await expect(game.locator('canvas')).toBeVisible({ timeout: 120_000 });

    await page.keyboard.press('F11');
    await expect.poll(() => page.evaluate(() => (window as any).__calls)).toContain('fullscreen:game');

    await game.locator('canvas').click();
    await expect.poll(() => page.evaluate(() => (window as any).__calls)).toContain('pointerlock:canvas');
    await expect(page.getByTestId('game-overlay')).toBeHidden();

    await page.evaluate(() => (window as any).__unlock()); // what the browser does on Esc
    const overlay = page.getByTestId('game-overlay');
    await expect(overlay).toBeVisible();
    await expect(overlay.getByTestId('leave')).toBeVisible();
    await expect(overlay.getByTestId('fullscreen')).toBeVisible();

    await game.locator('canvas').click(); // resume
    await expect(overlay).toBeHidden();
  } finally {
    await ctx.close();
    await api.deleteRoom(room.id);
  }
});

test('Connect maximizes the game and shows a staged loading screen until in game', async () => {
  const room = await api.createRoom('de_dust2', 'classic');
  const ctx = await profile({ width: 1024, height: 768 });
  try {
    const page = await ctx.newPage();
    await page.addInitScript(() => {
      const w = window as any;
      w.__calls = [] as string[];
      w.__stages = [] as string[];
      Element.prototype.requestFullscreen = function () {
        w.__calls.push('fullscreen:' + (this as Element).getAttribute('data-testid'));
        return Promise.resolve();
      };
      new MutationObserver(() => {
        const s = document.querySelector('[data-testid=loading-stage]')?.textContent?.toLowerCase() ?? '';
        if (s && w.__stages[w.__stages.length - 1] !== s) w.__stages.push(s);
      }).observe(document, { subtree: true, childList: true, characterData: true });
    });
    await page.goto('/');
    await page.getByTestId('name-input').fill(uniqueName('load'));
    await page.locator(`[data-testid=room-list] [data-room-id="${room.id}"] [data-testid=connect]`).click();
    // fullscreen must be requested within the click gesture, i.e. before bundles finish downloading
    expect(await page.evaluate(() => (window as any).__calls)).toContain('fullscreen:game');
    const loading = page.getByTestId('loading-screen');
    await expect(loading).toBeVisible();
    await expect(loading).toContainText('de_dust2');
    await expect(loading).toBeHidden({ timeout: 180_000 });
    const stages: string[] = await page.evaluate(() => (window as any).__stages);
    const order = ['download', 'engine', 'joining'].map((k) => stages.findIndex((s) => s.includes(k)));
    expect(order.every((i) => i >= 0), `stages seen: ${JSON.stringify(stages)}`).toBe(true);
    expect([...order].sort((a, b) => a - b)).toEqual(order);
  } finally {
    await ctx.close();
    await api.deleteRoom(room.id);
  }
});

test('lobby shows only the server list and create card; the game appears only after Connect', async () => {
  const room = await api.createRoom('de_dust2', 'classic');
  const ctx = await profile({ width: 1024, height: 768 });
  try {
    const page = await ctx.newPage();
    await page.goto('/');
    await expect(page.getByTestId('room-list')).toBeVisible();
    await expect(page.getByTestId('create-room')).toBeVisible();
    await expect(page.getByTestId('game')).toBeHidden();
    expect(await page.evaluate(() => (window as any).xash)).toBeUndefined(); // the engine has not booted
    await page.getByTestId('name-input').fill(uniqueName('lobby'));
    await page.locator(`[data-testid=room-list] [data-room-id="${room.id}"] [data-testid=connect]`).click();
    await expect(page.getByTestId('game')).toBeVisible();
    await expect(page.getByTestId('room-list')).toBeHidden();
  } finally {
    await ctx.close();
    await api.deleteRoom(room.id);
  }
});
