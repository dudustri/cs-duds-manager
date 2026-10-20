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

let manifestOnce: Promise<Manifest> | undefined;
function manifest(): Promise<Manifest> {
  manifestOnce ??= fetch('/manifest.json', { cache: 'no-cache' }).then((r) => {
    if (!r.ok) throw new Error(`manifest.json: ${r.status}`);
    return r.json() as Promise<Manifest>;
  });
  manifestOnce.catch(() => (manifestOnce = undefined));
  return manifestOnce;
}

/**
 * Fetches the base bundle (if `withBase`) and the bundles of `maps`, only those: the catalog is too big to load
 * whole. Returns mount name ("base.pk3", "map_de_dust2.pk3", ...) -> bytes.
 */
export async function loadBundles(
  maps: string[],
  withBase: boolean,
  onProgress: (done: number, total: number) => void,
): Promise<Map<string, Uint8Array>> {
  const m = await manifest();
  const items: [string, Bundle][] = maps.map((name): [string, Bundle] => {
    if (!m.maps[name]) throw new Error(`the game files have no map ${name}`);
    return [`map_${name}.pk3`, m.maps[name]];
  });
  if (withBase) items.unshift(['base.pk3', m.base]);

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

  if (cache) { // forget bundles a new manifest no longer lists
    const live = new Set([m.base, ...Object.values(m.maps)].map(urlOf));
    for (const req of await cache.keys()) if (!live.has(req.url)) await cache.delete(req);
  }
  return out;
}
