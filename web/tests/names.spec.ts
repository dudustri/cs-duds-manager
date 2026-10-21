// Names are unique: a name someone holds shows a toast; a free one turns the box green and offers Set.
import { expect, test } from '@playwright/test';
import { profile, requireStack, setPlayerName, uniqueName } from './fixtures.ts';

requireStack();

test('a taken name is refused, a free one can be set', async () => {
  const ctxA = await profile();
  const ctxB = await profile();
  try {
    const [a, b] = [await ctxA.newPage(), await ctxB.newPage()];
    const name = uniqueName('dupe');
    await a.goto('/');
    await setPlayerName(a, name);

    await b.goto('/');
    const box = b.getByTestId('name-input');
    await box.fill(name.toUpperCase()); // names are case-insensitive
    await expect(b.getByTestId('name-toast')).toBeVisible();
    await expect(b.getByTestId('name-toast')).toContainText('already taken');
    await expect(box).toHaveClass(/taken/);
    await expect(b.getByTestId('name-set')).toBeHidden();

    await box.fill(`${name}2`);
    await expect(box).toHaveClass(/\bok\b/);
    await b.getByTestId('name-set').click();
    await expect(b.getByTestId('name-set')).toBeHidden();
    await expect(box).not.toHaveClass(/\bok\b/);

    await b.reload(); // the claim survives a reload, and A still holds the first name
    await expect(box).toHaveValue(`${name}2`);
    await a.reload();
    await expect(a.getByTestId('name-input')).toHaveValue(name);
  } finally {
    await ctxA.close();
    await ctxB.close();
  }
});
