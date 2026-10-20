// Phase 2 exit (client half): a fresh profile downloads each bundle once; a reload is served from cache.
import { createHash } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { STACK, profile, requireStack } from './fixtures.ts';

requireStack();

interface Bundle { file: string; sha256: string; size: number }
interface Manifest { base: Bundle; maps: Record<string, Bundle> }

const EMPTY_ZIP = Buffer.concat([Buffer.from('504b0506', 'hex'), Buffer.alloc(18)]); // valid, empty pk3
const ready = (page: import('@playwright/test').Page, timeout: number) =>
  page.waitForFunction(() => Boolean((window as any).xash), undefined, { timeout });

test('bundles download once, reload is served from cache, manifest changes refetch only the changed bundle', async () => {
  const manifest = (await (await fetch(`${STACK}/manifest.json`)).json()) as Manifest;
  const bundles = [manifest.base, ...Object.values(manifest.maps)];
  const ctx = await profile();
  try {
    const page = await ctx.newPage();
    const assetRequests: string[] = [];
    page.on('request', (r) => {
      const p = new URL(r.url()).pathname;
      if (p.startsWith('/assets/')) assetRequests.push(p);
    });

    // first visit: boot to the menu (no connect)
    await page.goto('/?boot');
    await ready(page, 600_000);
    for (const b of bundles) expect(assetRequests.filter((p) => p.endsWith(b.file)), b.file).toHaveLength(1);

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

    // a changed map bundle: only it is fetched, and the stale cache entry is pruned
    const [mapName, old] = Object.entries(manifest.maps)[0];
    const sha = createHash('sha256').update(EMPTY_ZIP).digest('hex');
    const patched: Manifest = { ...manifest, maps: { ...manifest.maps, [mapName]: { file: `assets/${sha}.pk3`, sha256: sha, size: EMPTY_ZIP.length } } };
    await page.route('**/manifest.json', (r) => r.fulfill({ json: patched, headers: { 'cache-control': 'no-cache' } }));
    await page.route(`**/assets/${sha}.pk3`, (r) => r.fulfill({ body: EMPTY_ZIP, contentType: 'application/octet-stream' }));
    assetRequests.length = 0;
    await page.goto('/?boot');
    await ready(page, 60_000);
    expect(assetRequests).toEqual([`/assets/${sha}.pk3`]);
    const cached = await page.evaluate(async () => {
      const urls: string[] = [];
      for (const name of await caches.keys()) for (const req of await (await caches.open(name)).keys()) urls.push(new URL(req.url).pathname);
      return urls;
    });
    expect(cached).toContain(`/assets/${sha}.pk3`);
    expect(cached).not.toContain(`/${old.file}`);
  } finally {
    await ctx.close();
  }
});
