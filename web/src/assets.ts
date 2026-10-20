// Game content: manifest.json -> content-addressed pk3 bundles in CacheStorage, sha256-verified.

export interface Bundle { file: string; sha256: string; size: number }
export interface Manifest { base: Bundle; maps: Record<string, Bundle> }

const CACHE = 'cs-bundles';
const PARALLEL = 3;

const hex = (buf: ArrayBuffer): string => [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, '0')).join('');
const urlOf = (b: Bundle): string => new URL(b.file, document.baseURI).href;

/** Fetches a bundle, reporting bytes as they arrive. */
async function download(url: string, onBytes: (n: number) => void): Promise<Uint8Array<ArrayBuffer>> {
  const res = await fetch(url);
  if (!res.ok || !res.body) throw new Error(`download failed: ${url} (${res.status})`);
  const chunks: Uint8Array[] = [];
  let len = 0;
  const reader = res.body.getReader();
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
    len += value.length;
    onBytes(value.length);
  }
  const out = new Uint8Array(len);
  let at = 0;
  for (const c of chunks) (out.set(c, at), (at += c.length));
  return out;
}

/** Returns mount name ("base.pk3", "map_de_dust2.pk3", ...) -> bytes. */
export async function loadBundles(onProgress: (done: number, total: number) => void): Promise<Map<string, Uint8Array>> {
  const mres = await fetch('/manifest.json', { cache: 'no-cache' });
  if (!mres.ok) throw new Error(`manifest.json: ${mres.status}`);
  const manifest = (await mres.json()) as Manifest;
  const items: [string, Bundle][] = [['base.pk3', manifest.base], ...Object.entries(manifest.maps).map(([m, b]): [string, Bundle] => [`map_${m}.pk3`, b])];

  const secure = typeof caches !== 'undefined' && typeof crypto !== 'undefined' && !!crypto.subtle;
  if (!secure) console.warn('Not a secure context: game files are neither verified nor cached. Use https or localhost.');
  const cache = secure ? await caches.open(CACHE) : undefined;

  const total = items.reduce((n, [, b]) => n + b.size, 0);
  let done = 0;
  onProgress(0, total);
  const out = new Map<string, Uint8Array>();

  const fetchOne = async ([name, b]: [string, Bundle]): Promise<void> => {
    const url = urlOf(b);
    const hit = await cache?.match(url);
    if (hit) {
      out.set(name, new Uint8Array(await hit.arrayBuffer()));
      onProgress((done += b.size), total);
      return;
    }
    let got = 0;
    const bytes = await download(url, (n) => ((got += n), onProgress(done + got, total)));
    if (secure && hex(await crypto.subtle.digest('SHA-256', bytes)) !== b.sha256) {
      throw new Error(`${b.file}: sha256 mismatch`);
    }
    await cache?.put(url, new Response(bytes));
    out.set(name, bytes);
    onProgress((done += b.size), total);
  };

  const queue = [...items];
  await Promise.all(
    Array.from({ length: PARALLEL }, async () => {
      for (let it = queue.shift(); it; it = queue.shift()) await fetchOne(it);
    }),
  );

  if (cache) {
    const live = new Set(items.map(([, b]) => urlOf(b)));
    for (const req of await cache.keys()) if (!live.has(req.url)) await cache.delete(req);
  }
  return out;
}
