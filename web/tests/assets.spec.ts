// Phase 2 exit (client half), on-demand maps: the base bundle downloads once at boot, a map bundle only when a room
// on that map is joined, a reload is served from cache, and a manifest change prunes the stale cache entry.
import { createHash } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { STACK, api, joinFromList, profile, requireStack, uniqueName, waitForPlayers } from './fixtures.ts';

requireStack();

interface Bundle { file: string; sha256: string; size: number }
interface Manifest { base: Bundle; maps: Record<string, Bundle> }

const EMPTY_ZIP = Buffer.concat([Buffer.from('504b0506', 'hex'), Buffer.alloc(18)]); // valid, empty pk3
const ready = (page: import('@playwright/test').Page, timeout: number) =>
  page.waitForFunction(() => Boolean((window as any).xash), undefined, { timeout });

test('base downloads once, maps only when joined, reload is served from cache, a manifest change prunes the stale bundle', async () => {
  const manifest = (await (await fetch(`${STACK}/manifest.json`)).json()) as Manifest;
  const dust2 = manifest.maps.de_dust2;
  const room = await api.createRoom('de_dust2', 'classic');
  const ctx = await profile();
  try {
    const page = await ctx.newPage();
    const assetRequests: string[] = [];
    page.on('request', (r) => {
      const p = new URL(r.url()).pathname;
      if (p.startsWith('/assets/')) assetRequests.push(p);
    });

    // first visit: boot to the menu (no connect): the base bundle only
    await page.goto('/?boot');
    await ready(page, 600_000);
    expect(assetRequests).toEqual([`/${manifest.base.file}`]);

    // reload: nothing from /assets, every /engine resource from cache, engine ready fast
    assetRequests.length = 0;
    const t0 = Date.now();
    await page.reload();
    await ready(page, 5000);
    expect(Date.now() - t0).toBeLessThan(5000);
    expect(assetRequests).toEqual([]);
    const engine = await page.evaluate(() => performance.getEntriesByType('resource').filter((e) => e.name.includes('/engine/')).map((e) => (e as PerformanceResourceTiming).transferSize));
    expect(engine.length).toBeGreaterThan(0);
    expect(engine.every((s) => s === 0)).toBe(true);

    // joining a de_dust2 room fetches that map's bundle, and only it
    assetRequests.length = 0;
    const name = uniqueName('assets');
    await joinFromList(page, room.id, name);
    await waitForPlayers(room, [name]);
    expect(assetRequests).toEqual([`/${dust2.file}`]);

    // a new manifest changes de_dust2: nothing is fetched until a room needs it, and the stale entry is pruned
    const sha = createHash('sha256').update(EMPTY_ZIP).digest('hex');
    const patched: Manifest = { ...manifest, maps: { ...manifest.maps, de_dust2: { file: `assets/${sha}.pk3`, sha256: sha, size: EMPTY_ZIP.length } } };
    await page.route('**/manifest.json', (r) => r.fulfill({ json: patched, headers: { 'cache-control': 'no-cache' } }));
    assetRequests.length = 0;
    await page.goto('/?boot');
    await ready(page, 60_000);
    expect(assetRequests).toEqual([]);
    const cached = await page.evaluate(async () => {
      const urls: string[] = [];
      for (const n of await caches.keys()) for (const req of await (await caches.open(n)).keys()) urls.push(new URL(req.url).pathname);
      return urls;
    });
    expect(cached).toContain(`/${manifest.base.file}`);
    expect(cached).not.toContain(`/${dust2.file}`);
  } finally {
    await ctx.close();
    await api.deleteRoom(room.id);
  }
});
