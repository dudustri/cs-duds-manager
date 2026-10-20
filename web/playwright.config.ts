import { defineConfig } from '@playwright/test';

// Stack-tier browser tests. STACK_URL (e.g. http://localhost) points at the running compose stack;
// without it every spec skips (see requireStack() in fixtures.ts). The Chromium flags live in
// fixtures.ts (CHROMIUM_ARGS) because the specs use persistent contexts: on-disk profiles survive a
// 256 MiB wasm heap + large storage, in-memory ones crashed Chromium on reload in prior art.
export default defineConfig({
  testDir: './tests',
  testMatch: '*.spec.ts',
  timeout: 10 * 60_000,
  workers: 1,
  fullyParallel: false,
  reporter: [['list']],
  use: { baseURL: process.env.STACK_URL, browserName: 'chromium' },
  projects: [
    {
      name: 'chromium',
      use: { launchOptions: { args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'] } },
    },
  ],
});
