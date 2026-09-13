const assert = {
  equal<T>(actual: T, expected: T, message?: string): void {
    if (actual !== expected) throw new Error(`${message ?? "assertion failed"}: ${String(actual)} !== ${String(expected)}`);
  },
  deepEqual(actual: unknown, expected: unknown, message?: string): void {
    if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${message ?? "deep equality failed"}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`);
  },
  ok(value: boolean, message?: string): void { if (!value) throw new Error(message ?? "assertion failed"); },
  throws(run: () => void, pattern: RegExp): void {
    try { run(); } catch (error) {
      if (pattern.test(error instanceof Error ? error.message : String(error))) return;
    }
    throw new Error(`expected error matching ${pattern}`);
  },
};

import {
  DEFAULT_UNIFIED_THEME,
  UNIFIED_THEME_IDS,
  contrastRatio,
  isUnifiedThemeID,
  unifiedTheme,
} from "../src/unified_themes";

assert.deepEqual(UNIFIED_THEME_IDS, [
  "default",
  "rose-pine",
  "rose-pine-dawn",
  "solarized-dark",
  "gruvbox-dark",
  "one-dark",
  "dracula",
  "catppuccin-mocha",
]);
assert.equal(DEFAULT_UNIFIED_THEME, "default");

for (const id of UNIFIED_THEME_IDS) {
  assert.equal(isUnifiedThemeID(id), true, `${id} was not accepted by the closed theme grammar`);
  const palette = unifiedTheme(id);
  assert.equal(palette.id, id);
  assert.equal(palette.ansi.length, 16, `${id} does not provide the full ANSI palette`);
  assert.equal(new Set(palette.ansi).size, 16, `${id} aliases ANSI colours`);
  assert.ok(contrastRatio(palette.foreground, palette.background) >= 4.5, `${id} foreground contrast is below 4.5:1`);
  assert.ok(contrastRatio(palette.selectionForeground, palette.selectionBackground) >= 4.5, `${id} selection contrast is below 4.5:1`);
  assert.ok(contrastRatio(palette.selectionBackground, palette.background) >= 1.25, `${id} selection is not distinguishable from the field`);
  assert.equal(palette.xterm.background, palette.background);
  assert.equal(palette.xterm.foreground, palette.foreground);
  assert.equal(palette.xterm.selectionBackground, palette.selectionBackground);
  assert.equal(palette.xterm.selectionForeground, palette.selectionForeground);
}

for (const value of ["", "dark", "Default", "rose_pine", "catppuccin"]) {
  assert.equal(isUnifiedThemeID(value), false, `${value} escaped the closed theme grammar`);
}

assert.throws(() => unifiedTheme("dark" as never), /unknown unified theme/);
