// Browser side of the relay: engine datagrams <-> one unordered WebRTC data channel.
// Syntax is erasable-only TypeScript so Node 24 can run this file by type stripping.

import { openSession } from './api.ts';

export interface Packet {
  data: Int8Array;
  ip: [number, number, number, number];
  port: number;
}

export interface NetLike {
  incoming: { enqueue(p: Packet): void; clear?(): void };
}

/** The slice of RTCDataChannel the transport uses. */
export interface ChannelLike {
  binaryType: string;
  readyState: string;
  bufferedAmount: number;
  send(data: ArrayBuffer | ArrayBufferView): void;
  close(): void;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onclose: (() => void) | null;
}

/** The virtual server address the engine connects to; the relay maps it to the room. */
export const ENGINE_SERVER = { ip: [192, 0, 2, 1] as [number, number, number, number], port: 27015 };
export const CONNECT_CMD = 'connect 192.0.2.1:27015';

export class RelayTransport {
  net: NetLike | undefined;
  /** Vote token of the open session (X-Session-Token). */
  sessionToken = '';
  onclose: (reason: string) => void = () => {};
  #pc: { close(): void } | undefined;
  #channel: ChannelLike | undefined;

  constructor(deps: { net?: NetLike } = {}) {
    this.net = deps.net;
  }

  /** Wire an open channel: sets binaryType 'arraybuffer' and routes incoming messages to net.incoming. */
  attach(channel: ChannelLike): void {
    this.#channel = channel;
    channel.binaryType = 'arraybuffer';
    channel.onmessage = (ev) => {
      if (channel !== this.#channel) return;
      this.net?.incoming.enqueue({
        ip: [...ENGINE_SERVER.ip],
        port: ENGINE_SERVER.port,
        data: new Int8Array(ev.data as ArrayBuffer),
      });
    };
    channel.onclose = () => {
      if (channel !== this.#channel) return;
      this.#channel = undefined;
      this.onclose('channel closed');
    };
  }

  /** Engine callback with a heap VIEW: drop unless addressed to ENGINE_SERVER; copy before sending. */
  sendto(p: Packet): void {
    const ch = this.#channel;
    if (!ch || ch.readyState !== 'open') return;
    if (p.port !== ENGINE_SERVER.port || p.ip.some((b, i) => b !== ENGINE_SERVER.ip[i])) return;
    ch.send(p.data.slice()); // slice() copies; the heap memory is reused after we return
  }

  /** Non-trickle offer/answer through POST /api/rooms/{id}/session; resolves when "game" opens. */
  async open(roomId: string, auth?: string): Promise<void> {
    this.close();
    this.net?.incoming.clear?.(); // drop datagrams left over from the previous session
    const pc = new RTCPeerConnection();
    this.#pc = pc;
    try {
      const channel = pc.createDataChannel('game', { ordered: false, maxRetransmits: 0 });
      const opened = new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error('data channel open timed out')), 15_000);
        channel.onopen = () => (clearTimeout(timer), resolve());
        channel.onerror = () => (clearTimeout(timer), reject(new Error('data channel error')));
      });
      opened.catch(() => {}); // avoid an unhandled rejection if an earlier await throws first
      await pc.setLocalDescription(await pc.createOffer());
      if (pc.iceGatheringState !== 'complete') {
        await new Promise<void>((resolve) => {
          pc.addEventListener('icegatheringstatechange', () => pc.iceGatheringState === 'complete' && resolve());
          setTimeout(resolve, 5_000); // a stalled gatherer: offer the candidates we have rather than hang the page
        });
      }
      const { answer, token } = await openSession(roomId, pc.localDescription!.sdp, auth);
      await pc.setRemoteDescription({ type: 'answer', sdp: answer });
      await opened;
      if (this.#pc !== pc) throw new Error('superseded by a newer session');
      this.sessionToken = token;
      this.attach(channel as unknown as ChannelLike);
    } catch (e) {
      if (this.#pc === pc) this.close();
      else pc.close();
      throw e;
    }
  }

  close(): void {
    this.sessionToken = '';
    const ch = this.#channel;
    const pc = this.#pc;
    this.#channel = undefined;
    this.#pc = undefined;
    if (ch) ch.onmessage = ch.onclose = null;
    ch?.close();
    pc?.close();
  }
}
