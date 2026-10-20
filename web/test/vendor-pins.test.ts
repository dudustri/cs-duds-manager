import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// web/vendor/PINS.json: { "<tarball file name>": "<sha256 hex>", ... } for every file in web/vendor/*.tgz.
// Expected hashes come from grounding.md (npm xash3d-fwgs@1.2.2, cs16-client@0.1.2 from npmmirror).
const vendor = (f: string) => fileURLToPath(new URL(`../vendor/${f}`, import.meta.url));
const EXPECTED: Record<string, string> = {
  'xash3d-fwgs-1.2.2.tgz': '2f6c404dd8a64757e35eeb5aa4c968e0fa471d2972c338070e0cc4b020998606',
  'cs16-client-0.1.2.tgz': '2389bdaa5a8929918dedac6c548fc1c2ecfe1b78fdec48b654ebebcb1638d55f',
};

test('PINS.json pins the two engine tarballs to the grounded hashes', () => {
  const pins = JSON.parse(readFileSync(vendor('PINS.json'), 'utf8')) as Record<string, string>;
  for (const [file, sha] of Object.entries(EXPECTED)) assert.equal(pins[file], sha, `${file} pin`);
});

test('every pinned tarball is vendored and matches its sha256', () => {
  const pins = JSON.parse(readFileSync(vendor('PINS.json'), 'utf8')) as Record<string, string>;
  assert.ok(Object.keys(pins).length >= 2);
  for (const [file, sha] of Object.entries(pins)) {
    const actual = createHash('sha256').update(readFileSync(vendor(file))).digest('hex');
    assert.equal(actual, sha, `${file} on disk`);
  }
});
