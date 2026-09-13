"use strict";
const fs = require("fs");
const path = require("path");
const { spawnSync } = require("child_process");
const UI = path.resolve(__dirname, "..");
const ROOT = process.env.PERSEA_KEYS_EVIDENCE || require("node:path").join(require("node:os").tmpdir(), "persea-terminal-tests", "run_terminal_keys_mutants");
const esbuild = require(path.join(UI, "node_modules/esbuild"));
const mutants = [
  { name: "panel-gap-loses-focus", file: "terminal_keys_panel.ts", from: "if (document.activeElement instanceof HTMLTextAreaElement && !this.element.contains(document.activeElement)) event.preventDefault();", to: "void document.activeElement;", expected: "Native panel gap preserves focus, draft and selection" },
  { name: "short-panel-consumes-stage", file: "unified_terminal_page.ts", from: "const compact = this.keysPanel.open && this.keysUseStrip();", to: "const compact = false;", expected: "Composer and Keys preserve terminal content" },
  { name: "duplicate-key", file: "unified_terminal_page.ts", from: "else if (plan.descriptor) emitted = this.dispatchSyntheticKey(plan.descriptor);", to: "else if (plan.descriptor) { this.dispatchSyntheticKey(plan.descriptor); emitted = this.dispatchSyntheticKey(plan.descriptor); }", expected: "Repeated favorite emits one byte each" },
  { name: "native-text-canceled", file: "unified_terminal_page.ts", from: "      this.keysPanel.disarmForNativeInput();", to: "      event.preventDefault();\n      this.keysPanel.disarmForNativeInput();", expected: "Combined short layout preserves draft, focus and selection" },
  { name: "lifecycle-panel-retained", file: "unified_terminal_page.ts", from: "    this.keysPanel?.cancel();", to: "    // mutant retains panel", expected: "Transport closes pending panel state" },
  { name: "preferences-not-saved", file: "keyboard_preferences.ts", from: "this.storage.setItem(DEVICE_KEYBOARD_PREFERENCES_KEY, JSON.stringify(device));", to: "void DEVICE_KEYBOARD_PREFERENCES_KEY;", expected: "User-defined provider prefix persists independently" },
  { name: "wrong-f12", file: "unified_key_bar.ts", from: 'f12: { key: "F12", code: "F12", keyCode: 123 }', to: 'f12: { key: "F12", code: "F12", keyCode: 122 }', expected: "Catalog f12" },
];
function assert(value, message) { if (!value) throw new Error(message); }
async function main() {
  fs.mkdirSync(ROOT, { recursive: true });
  const results = [];
  for (const mutant of [null, ...mutants]) {
    const name = mutant?.name || "clean";
    const root = path.join(ROOT, `keys-mutant-${name}`);
    fs.mkdirSync(root, { recursive: true });
    fs.cpSync(path.join(UI, "dist"), path.join(root, "dist"), { recursive: true });
    if (!fs.existsSync(path.join(root, "node_modules"))) fs.symlinkSync(path.join(UI, "node_modules"), path.join(root, "node_modules"), "dir");
    let touches = 0;
    await esbuild.build({
      entryPoints: [path.join(UI, "src/app.ts")], bundle: true, format: "esm", platform: "browser", outfile: path.join(root, "dist/app.js"), logLevel: "silent",
      plugins: mutant ? [{ name: "keys-mutant", setup(build) {
        build.onLoad({ filter: /\.ts$/ }, (args) => {
          if (path.basename(args.path) !== mutant.file) return;
          const source = fs.readFileSync(args.path, "utf8");
          const pieces = source.split(mutant.from);
          assert(pieces.length === 2, `${name}: mutation site count ${pieces.length - 1}`);
          touches++;
          return { contents: pieces.join(mutant.to), loader: "ts", resolveDir: path.dirname(args.path) };
        });
      } }] : [],
    });
    // The catalog is independently bundled and must see the same encoder mutant.
    await esbuild.build({ entryPoints: [path.join(UI, "test/terminal_keys_catalog_harness.ts")], bundle: true, format: "iife", platform: "browser", outfile: path.join(root, "dist/test/terminal_keys_catalog_harness.js"), logLevel: "silent", plugins: mutant?.name === "wrong-f12" ? [{ name: "catalog-mutant", setup(build) { build.onLoad({ filter: /unified_key_bar\.ts$/ }, (args) => ({ contents: fs.readFileSync(args.path, "utf8").replace(mutant.from, mutant.to), loader: "ts", resolveDir: path.dirname(args.path) })); } }] : [] });
    assert(!mutant || touches === 1, `${name}: mutation was not compiled`);
    const run = spawnSync(process.execPath, [path.join(__dirname, "run_terminal_keys_browser.cjs")], { cwd: UI, encoding: "utf8", timeout: 60000, env: { ...process.env, PERSEA_KEYS_UI: root, PERSEA_KEYS_ENGINE: "chromium", PERSEA_KEYS_ONE_SHAPE: "1", PERSEA_KEYS_EVIDENCE: root } });
    const output = `${run.stdout || ""}${run.stderr || ""}`;
    fs.writeFileSync(path.join(root, "runner.log"), output);
    assert(!run.error && !run.signal, `${name}: infrastructure failure`);
    assert(mutant ? run.status === 1 && output.includes(mutant.expected) : run.status === 0, `${name}: did not reach intended ${mutant?.expected || "PASS"} assertion: ${output.slice(-1000)}`);
    results.push({ name, compiled: true, exit: run.status, expected: mutant?.expected || "PASS" });
    console.log(`Keys causal control ${name}: PASS`);
  }
  fs.writeFileSync(path.join(ROOT, "keys-mutants.json"), JSON.stringify(results, null, 2));
}
main().catch((error) => { console.error(error.stack || String(error)); process.exitCode = 1; });
