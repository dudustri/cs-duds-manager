// Builds web/dist: bundled app, static pages, and the engine runtime files unpacked from web/vendor/*.tgz (no network).
import { build } from 'esbuild';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { cpSync, mkdtempSync, readdirSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

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
  const tmp = mkdtempSync(join(tmpdir(), 'vendor-'));
  try {
    execFileSync('tar', ['-xzf', join(root, 'vendor', file), '-C', tmp, '--no-same-owner']);
    const src = join(tmp, 'package/dist');
    for (const rel of readdirSync(src, { recursive: true })) {
      if (KEEP.test(rel) && !SKIP.test(rel)) cpSync(join(src, rel), join(dist, 'engine', name, rel));
    }
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
}
