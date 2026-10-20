import { test } from 'node:test';
import assert from 'node:assert/strict';
import { CONNECT_CMD, ENGINE_SERVER, RelayTransport } from '../src/transport.ts';
import type { ChannelLike, Packet } from '../src/transport.ts';

// A fake RTCDataChannel that RETAINS what it was given, so a missing copy is observable.
function fakeChannel() {
  const sent: unknown[] = [];
  const ch: ChannelLike = {
    binaryType: 'blob',
    readyState: 'open',
    bufferedAmount: 0,
    send: (d) => void sent.push(d),
    close() {},
    onmessage: null,
    onclose: null,
  };
  return { ch, sent };
}

function fakeNet() {
  const got: Packet[] = [];
  return { net: { incoming: { enqueue: (p: Packet) => void got.push(p) } }, got };
}

const bytesOf = (d: unknown): number[] => {
  const v = d as ArrayBufferView & ArrayBuffer;
  return [...new Uint8Array(v.buffer ?? v, v.byteOffset ?? 0, v.byteLength)];
};

function setup() {
  const { ch, sent } = fakeChannel();
  const { net, got } = fakeNet();
  const t = new RelayTransport({ net });
  t.attach(ch);
  return { t, ch, sent, got };
}

test('the engine server is the virtual TEST-NET-1 address', () => {
  assert.deepEqual(ENGINE_SERVER, { ip: [192, 0, 2, 1], port: 27015 });
  assert.equal(CONNECT_CMD, 'connect 192.0.2.1:27015');
});

test('packets to any other address are dropped', () => {
  const { t, sent } = setup();
  const data = new Int8Array([1, 2, 3]);
  for (const [ip, port] of [
    [[255, 255, 255, 255], 27015], // LAN broadcast
    [[192, 0, 2, 1], 27016],
    [[192, 0, 2, 2], 27015],
    [[127, 0, 0, 1], 27015],
    [[8, 8, 8, 8], 27010], // master query
  ] as const) {
    t.sendto({ data, ip: [...ip] as Packet['ip'], port });
  }
  assert.equal(sent.length, 0);
  t.sendto({ data, ip: ENGINE_SERVER.ip, port: ENGINE_SERVER.port });
  assert.equal(sent.length, 1);
  assert.deepEqual(bytesOf(sent[0]), [1, 2, 3]);
});

test('sendto copies the wasm-heap view (the engine reuses that memory)', () => {
  const { t, sent } = setup();
  const heap = new Int8Array(64);
  heap.set([-1, -1, -1, -1, 0x57, 0x68], 8);
  const view = heap.subarray(8, 14); // what the engine passes: a view into the heap
  t.sendto({ data: view, ip: ENGINE_SERVER.ip, port: ENGINE_SERVER.port });
  heap.fill(0x7f); // the engine overwrites the heap after sendto returns
  assert.deepEqual(bytesOf(sent[0]), [0xff, 0xff, 0xff, 0xff, 0x57, 0x68]);
});

test('an incoming datagram is enqueued as coming from the engine server', () => {
  const { t, ch, got } = setup();
  assert.equal(ch.binaryType, 'arraybuffer');
  const payload = new Uint8Array([0xff, 0xff, 0xff, 0xff, 0x6c, 0x80]);
  ch.onmessage!({ data: payload.buffer.slice(0) });
  assert.equal(got.length, 1);
  assert.deepEqual(got[0].ip, [192, 0, 2, 1]);
  assert.equal(got[0].port, 27015);
  assert.deepEqual([...got[0].data], [-1, -1, -1, -1, 0x6c, -128]);
  assert.ok(t);
});
