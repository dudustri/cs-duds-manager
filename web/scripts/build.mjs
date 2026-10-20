// Builds web/dist: bundled app, static pages, and the engine runtime files unpacked from web/vendor/*.tgz (no network).
import { build } from 'esbuild';
import { createHash } from 'node:crypto';
import { cpSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gunzipSync } from 'node:zlib';

const root = fileURLToPath(new URL('..', import.meta.url));
const dist = join(root, 'dist');
const pins = JSON.parse(readFileSync(join(root, 'vendor/PINS.json'), 'utf8'));

rmSync(dist, { recursive: true, force: true });

await build({
  entryPoints: { main: 'src/main.ts', admin: 'src/admin.ts', 'clock.worker': 'src/clock.worker.ts', app: 'src/app.css' },
  outdir: join(dist, 'app'),
  absWorkingDir: root,
  bundle: true,
  format: 'esm',
  target: 'es2022',
  minify: true,
  sourcemap: true,
  logLevel: 'info',
});
for (const page of ['index.html', 'admin.html']) cpSync(join(root, page), join(dist, page));

// Files the page loads at runtime; yapb (bots), libmenu and the soft renderer are not used.
const KEEP = /\.(wasm|pk3)$/;
const SKIP = /(yapb|libmenu|libref_soft)/;

for (const [file, sha] of Object.entries(pins)) {
  const buf = readFileSync(join(root, 'vendor', file));
  if (createHash('sha256').update(buf).digest('hex') !== sha) throw new Error(`${file}: sha256 differs from PINS.json`);
  const name = file.replace(/-(\d[^-]*)\.tgz$/, '@$1'); // xash3d-fwgs-1.2.2.tgz -> xash3d-fwgs@1.2.2
  const tar = gunzipSync(buf);
  for (let off = 0; off + 512 <= tar.length; ) {
    const h = tar.subarray(off, off + 512);
    if (h.every((b) => b === 0)) break;
    const field = (a, n) => h.toString('latin1', a, a + n).replace(/\0.*$/s, '');
    const size = parseInt(field(124, 12).trim() || '0', 8);
    const path = field(0, 100);
    const rel = path.replace(/^package\/dist\//, '');
    if (field(156, 1) === '0' && rel !== path && KEEP.test(rel) && !SKIP.test(rel)) {
      const out = join(dist, 'engine', name, rel);
      mkdirSync(dirname(out), { recursive: true });
      writeFileSync(out, tar.subarray(off + 512, off + 512 + size));
    }
    off += 512 + Math.ceil(size / 512) * 512;
  }
}
