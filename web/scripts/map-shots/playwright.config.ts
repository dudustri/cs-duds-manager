// Run with: npx playwright test -c scripts/map-shots   (needs the stack up, like the stack-tier specs)
import { defineConfig } from '@playwright/test';
export default defineConfig({ testDir: '.', timeout: 0, workers: 1, reporter: 'line' });
