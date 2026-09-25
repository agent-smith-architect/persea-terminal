import { UNIFIED_THEME_IDS } from "../src/unified_themes";

declare const require: (name: string) => unknown;
declare const process: { cwd(): string; stdout: { write(value: string): void } };
const fs = require("node:fs") as { readFileSync(path: string, encoding: "utf8"): string };
const path = require("node:path") as { join(...parts: string[]): string };

const source = fs.readFileSync(path.join(process.cwd(), "src", "unified_terminal_page.ts"), "utf8");
const css = ["attachment_shell", "attachment_composer", "attachment_surface", "unified_terminal_surface", "unified_terminal_toolbar", "unified_terminal_composer", "unified_terminal_overlays", "unified_terminal_disclosures"]
  .map((name) => fs.readFileSync(path.join(process.cwd(), "src", `${name}.css`), "utf8")).join("\n");
const assert = (value: boolean, message: string): void => { if (!value) throw new Error(message); };

function luminance(hex: string): number {
  const values = [1, 3, 5].map((index) => Number.parseInt(hex.slice(index, index + 2), 16) / 255)
    .map((value) => value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4);
  return 0.2126 * values[0] + 0.7152 * values[1] + 0.0722 * values[2];
}
function contrast(a: string, b: string): number {
  const values = [luminance(a), luminance(b)].sort((left, right) => right - left);
  return (values[0] + 0.05) / (values[1] + 0.05);
}

// The shared Clipboard replaces the retired saved-text/last-lines factories.
// Its actual preview, copy success/refusal and live feedback are covered by
// the Clipboard browser suite. The contextual toolbar keeps its own outcome
// state machine and these independent source/style/contrast fences.
// the contextual Copy outcome lives on the Paste slot.
assert(source.includes('button.dataset.pasteState = "copied"') && source.includes('this.pasteWord.textContent = "Copied"'), "contextual Copy lacks a local-success outcome");
assert(source.includes('button.dataset.pasteState = "failed"') && source.includes('this.pasteWord.textContent = "Failed"'), "contextual Copy lacks a local-failure outcome");
// The Paste slot's outcome colours are fixed pairs too (theme-contrast on every theme).
assert(css.includes('button.persea-unified-toolbar-paste[data-paste-state="copied"]') && css.includes('button.persea-unified-toolbar-paste[data-paste-state="failed"]'), "Paste slot outcome states are unstyled");
for (const theme of UNIFIED_THEME_IDS) {
  assert(contrast("#d8ffe4", "#143822") >= 4.5, `${theme}: Paste slot copied contrast fell below 4.5:1`);
  assert(contrast("#ffd9d9", "#3a1616") >= 4.5, `${theme}: Paste slot failed contrast fell below 4.5:1`);
}

// These !important outcome colours intentionally do not inherit a theme;
// testing all eight IDs makes that invariant explicit rather than assuming a
// default-theme contrast carries over.
assert(css.includes('button[data-copy-state="done"]') && css.includes("background: #237a3b !important") && css.includes("color: #ffffff !important"), "done colours are not the fixed outcome pair");
assert(css.includes('button[data-copy-state="failed"]') && css.includes("background: #b42318 !important") && css.includes("color: #ffffff !important"), "failed colours are not the fixed outcome pair");
for (const theme of UNIFIED_THEME_IDS) {
  assert(contrast("#ffffff", "#237a3b") >= 3, `${theme}: done contrast fell below 3:1`);
  assert(contrast("#ffffff", "#b42318") >= 3, `${theme}: failed contrast fell below 3:1`);
}

process.stdout.write("copy feedback affordance and eight-theme contrast fence: PASS\n");
