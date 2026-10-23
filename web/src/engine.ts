// Boots the browser engine we compile from source (Xash3D FWGS + cs16-client, see the engine stage in the Dockerfile)
// and wires its network layer to the relay.
import type { Packet, RelayTransport } from './transport.ts';

// Built by the Dockerfile's engine stage and served from /engine/<hash of its files>/ (not bundled: its own module).
declare const __ENGINE_PATH__: string;
const ENGINE = __ENGINE_PATH__;

const LIBLIST = 'game "Counter-Strike"\ntype "multiplayer_only"\nmpentity "info_player_start"\ngamedll "dlls\\hl.dll"\n';

/** Emscripten's in-memory file system, as far as we use it. */
interface EmFS {
  mkdirTree(path: string): void;
  writeFile(path: string, data: string | Uint8Array, opts?: { canOwn?: boolean }): void;
  chdir(path: string): void;
}

/** The running engine: what the rest of the page uses (and tests reach as window.xash). */
export interface Engine {
  Cmd_ExecuteString(cmd: string): void;
  FS: EmFS;
}

interface EmModule {
  FS: EmFS;
  ENV: Record<string, string>;
  callMain(args: string[]): void;
  ccall(name: string, ret: null, types: string[], args: unknown[]): void;
}

/** Datagrams from the relay, waiting for the engine's recvfrom (engine/net.js); oldest dropped when full, like UDP. */
class Inbox {
  #q: Packet[] = [];
  enqueue(p: Packet): void {
    if (this.#q.push(p) > 256) this.#q.shift();
  }
  pull(): Packet | undefined {
    return this.#q.shift();
  }
  clear(): void {
    this.#q.length = 0;
  }
}

/** Strips quotes, semicolons, backslashes and control characters; max 31 chars (the engine limit). */
export function cleanName(raw: string): string {
  return raw.replace(/[\x00-\x1f\x7f"';\\]/g, '').trim().slice(0, 31) || 'player';
}

export function setName(x: Engine, raw: string): void {
  x.Cmd_ExecuteString(`name "${cleanName(raw)}"`);
}

/** Boots once per page. `bundles` are the pk3 files from loadBundles(); `onLog` sees every engine console line. */
export async function bootEngine(
  canvas: HTMLCanvasElement,
  bundles: Map<string, Uint8Array>,
  transport: RelayTransport,
  onLog: (line: string) => void = () => {},
): Promise<Engine> {
  installHiddenTabClock();
  const inbox = new Inbox();
  transport.net = { incoming: inbox };
  const url = `${ENGINE}xash.js`; // a variable, so the bundler leaves this import to the browser
  const { default: createXash } = (await import(url)) as { default: (m: object) => Promise<EmModule> };
  const em = await createXash({
    canvas,
    net: { incoming: inbox, sendto: (p: Packet) => transport.sendto(p) }, // used by engine/net.js
    locateFile: (path: string) => ENGINE + path,
    // no executable path in a browser to find the game from; C reads the environment once, before main
    preRun: [(m: EmModule) => (m.ENV.XASH3D_BASEDIR = '/rodir')],
    print: (line: string) => (console.log(line), onLog(line)),
    printErr: (line: string) => (console.warn(line), onLog(line)),
  });

  const fs = em.FS;
  const fetchBytes = async (path: string) => {
    const r = await fetch(ENGINE + path);
    if (!r.ok) throw new Error(`${path}: ${r.status}`);
    return new Uint8Array(await r.arrayBuffer());
  };
  const put = async (path: string, bytes: Uint8Array) => fs.writeFile(path, bytes, { canOwn: true });
  fs.mkdirTree('/rodir/valve');
  fs.mkdirTree('/rodir/cstrike/cl_dlls');
  // the engine dlopens these .so files from its own folder and the game folder, like on a desktop; they are served
  // as .wasm so the web server compresses them
  await Promise.all(
    ['filesystem_stdio.so', 'libref_gles3compat.so', 'valve/extras.pk3', 'cstrike/extras.pk3',
      'cstrike/cl_dlls/client_emscripten_wasm32.so', 'cstrike/cl_dlls/menu_emscripten_wasm32.so']
      .map(async (f) => put(`/rodir/${f}`, await fetchBytes(f.replace(/\.so$/, '.wasm')))),
  );
  // The engine finds game directories by a real liblist.gam on disk; the copy inside base.pk3 is not enough.
  fs.writeFile('/rodir/cstrike/liblist.gam', LIBLIST);
  for (const [name, bytes] of bundles) await put(`/rodir/cstrike/${name}`, bytes);
  bundles.clear();

  fs.chdir('/rodir');
  const x: Engine = { FS: fs, Cmd_ExecuteString: (cmd) => em.ccall('Cmd_ExecuteString', null, ['string'], [cmd]) };
  // main() runs the engine loop; with ASYNCIFY it returns to us at the first frame's sleep and keeps going from there
  // -vao: draw from GPU buffers only, as WebGL requires; +cl_advertise_engine_in_name 0: no "[Xash3D]" name prefix
  em.callMain(['-game', 'cstrike', '-ref', 'gles3compat', '-vao', '+cl_advertise_engine_in_name', '0']);
  await new Promise<void>((resolve) => requestAnimationFrame(() => setTimeout(resolve, 0))); // let it finish one frame
  (window as unknown as { xash: Engine }).xash = x;
  return x;
}

/** Adds map bundles to a running engine (between maps: the rescan reopens every pack). */
export function mountMaps(x: Engine, bundles: Map<string, Uint8Array>): void {
  for (const [name, bytes] of bundles) x.FS.writeFile(`/rodir/cstrike/${name}`, bytes, { canOwn: true });
  bundles.clear();
  x.Cmd_ExecuteString('fs_rescan');
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
