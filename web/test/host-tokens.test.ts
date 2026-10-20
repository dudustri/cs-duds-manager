// hostTokens keeps only the newest 20 rooms in localStorage.
import assert from 'node:assert/strict';
import { test } from 'node:test';

const store = new Map<string, string>();
(globalThis as { localStorage?: unknown }).localStorage = {
  getItem: (k: string) => store.get(k) ?? null,
  setItem: (k: string, v: string) => void store.set(k, v),
};
const { hostTokens } = await import('../src/api.ts');

test('hostTokens keeps the newest 20', () => {
  for (let i = 0; i < 25; i++) hostTokens.set(`room${i}x`, `t${i}`);
  const saved = JSON.parse(store.get('cs-host-tokens')!);
  assert.equal(Object.keys(saved).length, 20);
  assert.equal(saved.room24x, 't24');
  assert.equal(saved.room4x, undefined);
  assert.equal(hostTokens.get('room24x'), 't24');
});
