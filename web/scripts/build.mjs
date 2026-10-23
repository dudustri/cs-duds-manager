// Builds web/dist: bundled app and static pages. The engine (/engine/<hash>/, ENGINE_PATH) is compiled from source by
// the Dockerfile's engine stage and added next to these in the caddy image.
import { build } from 'esbuild';
import { cpSync, readdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('..', import.meta.url));
const dist = join(root, 'dist');

rmSync(dist, { recursive: true, force: true });

// Loading-screen backgrounds: every image in img/loading/ (drop one in to add it), picked at random per load.
const loadingImages = readdirSync(join(root, 'img/loading')).filter((f) => /\.(webp|jpe?g|png)$/.test(f)).sort();

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
  external: ['/img/*'], // served as files next to the page (web/img, and the team art from the Dockerfile)
  define: {
    __LOADING_IMAGES__: JSON.stringify(loadingImages.map((f) => `/img/loading/${f}`)),
    __ENGINE_PATH__: JSON.stringify(process.env.ENGINE_PATH ?? '/engine/dev/'), // set by the Dockerfile's webbuild stage
  },
});
for (const page of ['index.html', 'admin.html']) cpSync(join(root, page), join(dist, page));
cpSync(join(root, 'img'), join(dist, 'img'), { recursive: true });
