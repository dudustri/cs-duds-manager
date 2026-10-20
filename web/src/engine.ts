// Boots the vendored Xash3D-FWGS wasm engine with CS 1.6 and wires its network layer to the relay.
import { Net, Xash3D } from 'xash3d-fwgs';
import type { RelayTransport } from './transport.ts';

// Runtime files are extracted from web/vendor/*.tgz into dist/engine/ by scripts/build.mjs.
const XASH = '/engine/xash3d-fwgs@1.2.2/';
const CS = '/engine/cs16-client@0.1.2/';

const LIBLIST = 'game "Counter-Strike"\ntype "multiplayer_only"\nmpentity "info_player_start"\ngamedll "dlls\\hl.dll"\n';

/** Strips quotes, semicolons, backslashes and control characters; max 31 chars (the engine limit). */
export function cleanName(raw: string): string {
  return raw.replace(/[\x00-\x1f\x7f"';\\]/g, '').trim().slice(0, 31) || 'player';
}

export function setName(x: Xash3D, raw: string): void {
  x.Cmd_ExecuteString(`name "${cleanName(raw)}"`);
}

/** Boots once per page. `bundles` are the pk3 files from loadBundles(); `onLog` sees every engine console line. */
export async function bootEngine(
  canvas: HTMLCanvasElement,
  bundles: Map<string, Uint8Array>,
  transport: RelayTransport,
  onLog: (line: string) => void = () => {},
): Promise<Xash3D> {
  installHiddenTabClock();
  const x = new Xash3D({
    canvas,
    arguments: ['-game', 'cstrike', '+cl_advertise_engine_in_name', '0'], // no "[Xash3D]" prefix on player names
    libraries: {
      xash: `${XASH}xash.wasm`,
      filesystem: `${XASH}filesystem_stdio.wasm`,
      menu: `${CS}cstrike/cl_dlls/menu_emscripten_wasm32.wasm`,
      client: `${CS}cstrike/cl_dlls/client_emscripten_wasm32.wasm`,
      server: `${CS}cstrike/dlls/cs_emscripten_wasm32.wasm`,
      render: { gles3compat: `${XASH}libref_webgl2.wasm` },
    },
    // Xash3D replaces print/printErr with wrappers that forward to these.
    module: {
      print: (line: string) => (console.log(line), onLog(line)),
      printErr: (line: string) => (console.warn(line), onLog(line)),
    } as never,
  });
  const net = new Net(transport);
  transport.net = net;
  x.net = net; // must be set before init()
  await x.init();

  const fs = x.em!.FS;
  fs.mkdirTree('/rodir/valve');
  fs.mkdirTree('/rodir/cstrike');
  const put = async (path: string, bytes: Uint8Array) => fs.writeFile(path, bytes, { canOwn: true });
  const fetchBytes = async (url: string) => {
    const r = await fetch(url);
    if (!r.ok) throw new Error(`${url}: ${r.status}`);
    return new Uint8Array(await r.arrayBuffer());
  };
  await put('/rodir/filesystem_stdio.wasm', await fetchBytes(`${XASH}filesystem_stdio.wasm`)); // the engine dlopens it by absolute path
  await put('/rodir/valve/extras.pk3', await fetchBytes(`${XASH}valve/extras.pk3`));
  await put('/rodir/cstrike/extras.pk3', await fetchBytes(`${CS}cstrike/extras.pk3`));
  // The engine finds game directories by a real liblist.gam on disk; the copy inside base.pk3 is not enough.
  fs.writeFile('/rodir/cstrike/liblist.gam', LIBLIST);
  for (const [name, bytes] of bundles) await put(`/rodir/cstrike/${name}`, bytes);
  bundles.clear();

  fs.chdir('/rodir');
  x.main();
  // main() runs asynchronously after start(); a command before that throws Infinity (an escaped longjmp), then
  // the engine needs one frame to finish initialising.
  const module = x.em!.Module as { calledRun?: boolean };
  while (!module.calledRun) await new Promise((resolve) => setTimeout(resolve, 20));
  await new Promise<void>((resolve) => requestAnimationFrame(() => setTimeout(resolve, 0)));
  (window as unknown as { xash: Xash3D }).xash = x;
  return x;
}

let clockInstalled = false;

/**
 * Hidden tabs stop requestAnimationFrame, which drives the engine loop, and the server drops a silent
 * client. While document.hidden, pending frame callbacks run on 50 ms ticks from a worker instead.
 */
export function installHiddenTabClock(): void {
  if (clockInstalled) return;
  clockInstalled = true;
  const native = window.requestAnimationFrame.bind(window);
  const pending = new Map<number, FrameRequestCallback>();
  let seq = 0;
  const run = (id: number, t: number) => {
    const cb = pending.get(id);
    if (cb) (pending.delete(id), cb(t));
  };
  window.requestAnimationFrame = (cb) => {
    const id = ++seq;
    pending.set(id, cb);
    if (!document.hidden) native((t) => run(id, t));
    return id;
  };
  window.cancelAnimationFrame = (id) => void pending.delete(id);

  new Worker(new URL('./clock.worker.js', import.meta.url)).onmessage = () => {
    if (document.hidden) for (const id of [...pending.keys()]) run(id, performance.now());
  };
  // Callbacks queued while hidden never got a native frame; hand them back when the tab returns.
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) for (const id of [...pending.keys()]) native((t) => run(id, t));
  });
}
