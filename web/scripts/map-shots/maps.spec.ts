// Takes the lobby's map pictures: for every catalog map, start a room, spawn as a Terrorist with the HUD, crosshair
// and weapon hidden (freeze time holds the spawn view) and capture the game canvas to web/img/maps/<map>.png
// (convert to .webp afterwards). It turns 90° four times and keeps the view with the most large-scale contrast
// (standard deviation of a 32x18 thumbnail: sky against ground scores high, a wall close-up goes flat).
// MAPS=a,b limits it to some maps.
import { execFileSync } from 'node:child_process';
import { mkdirSync, renameSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { test } from '@playwright/test';
import { STACK, api, joinFromList, profile, uniqueName, waitForPlayers } from '../../tests/fixtures.ts';

const OUT = join(import.meta.dirname, '../../img/maps');

test('map screenshots', async () => {
  mkdirSync(OUT, { recursive: true });
  const { maps } = (await (await fetch(`${STACK}/api/catalog`)).json()) as { maps: string[] };
  const only = process.env.MAPS?.split(',');
  const ctx = await profile({ width: 1280, height: 720 });
  try {
    for (const map of maps.filter((m) => !only || only.includes(m))) {
      const room = await api.createRoom(map, 'classic');
      try {
        const page = await ctx.newPage();
        const name = uniqueName('shot');
        await joinFromList(page, room.id, name);
        await waitForPlayers(room, [name]);
        await page.evaluate(() => {
          const x = (window as any).xash;
          for (const c of ['hud_draw 0', 'crosshair 0', 'r_drawviewmodel 0', 'hideradar', 'con_notifytime 0', 'hud_saytext_time 0'])
            x.Cmd_ExecuteString(c);
        });
        await page.getByTestId('team-t').click();
        await page.waitForTimeout(3000); // spawned, still frozen
        await page.evaluate(() => (document.getElementById('overlay')!.hidden = true));
        const shots: { file: string; score: number }[] = [];
        for (let k = 0; k < 4; k++) {
          const file = join(OUT, `${map}.${k}.png`);
          await page.locator('#canvas').screenshot({ path: file });
          const score = execFileSync('magick', [file, '-resize', '32x18!', '-colorspace', 'Gray', '-format', '%[fx:standard_deviation]', 'info:'], { encoding: 'utf8' });
          shots.push({ file, score: Number(score) });
          await page.evaluate(() => (window as any).xash.Cmd_ExecuteString('+right'));
          await page.waitForTimeout(430); // cl_yawspeed 210°/s: about a quarter turn
          await page.evaluate(() => (window as any).xash.Cmd_ExecuteString('-right'));
          await page.waitForTimeout(400);
        }
        const best = shots.reduce((a, b) => (b.score > a.score ? b : a));
        renameSync(best.file, join(OUT, `${map}.png`));
        for (const s of shots) if (s !== best) rmSync(s.file, { force: true });
        console.log(`shot ${map}`);
        await page.close();
      } finally {
        await api.deleteRoom(room.id);
      }
    }
  } finally {
    await ctx.close();
  }
});
