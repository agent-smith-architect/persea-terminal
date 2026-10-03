"use strict";

// Causal gate for run_terminal_position_browser.cjs. Each mutant breaks one
// part of terminal position in a private build; the browser test must fail on
// the assertion that names that part. A clean build made the same way must
// pass first, so a mutant's failure cannot be the harness's own.
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const UI = path.resolve(__dirname, "..");
const EVIDENCE = path.resolve(process.env.PERSEA_POSITION_EVIDENCE || path.join(os.tmpdir(), "persea-terminal-position-mutants"));
const ENGINE = process.env.PERSEA_POSITION_ENGINE || "chromium";
const esbuild = require(path.join(UI, "node_modules/esbuild"));

const CENTRED_LEFT = "left: calc((100% - var(--persea-unified-grid-width, 100%)) / 2);";
const MUTANTS = [
  {
    name: "horizontal-offset-ignored",
    summary: "a narrower grid stays at the left edge",
    case: "placement",
    edits: [{ file: "unified_terminal_toolbar.css", from: CENTRED_LEFT, to: "left: 0;" }],
    expected: "desktop small 36x10 top-center: grid is not placed horizontally as chosen",
  },
  {
    name: "vertical-offset-ignored",
    summary: "Center leaves a shorter grid at the top",
    case: "placement",
    edits: [{ file: "unified_terminal_page.ts", from: "this.gridOffsetY = centersVertically(this.terminalPosition) ? Math.floor(this.gridFreeHeight / 2) : 0;", to: "this.gridOffsetY = 0;" }],
    expected: "desktop small 36x10 center: grid is not placed vertically as chosen",
  },
  {
    // The centring is measured against the scroll range, which is never
    // narrower than the grid. Measured against the viewport instead, a wider
    // grid gets a negative offset and its first columns are clipped.
    name: "horizontal-offset-on-wider-grid",
    summary: "a wider grid is centred too, pushing its first columns out of reach",
    case: "placement",
    edits: [{ file: "unified_terminal_page.ts", from: "this.scrollRange.style.width = `${grid.width}px`;", to: "this.scrollRange.style.width = \"\";" }],
    expected: "desktop wide 200x10 top-center: grid is not placed horizontally as chosen",
  },
  {
    name: "vertical-offset-on-taller-grid",
    summary: "a taller grid is centred too, pushing its first rows out of reach",
    case: "placement",
    edits: [{ file: "unified_terminal_page.ts", from: "this.gridFreeHeight = Math.max(0, viewportHeight - grid.height);", to: "this.gridFreeHeight = viewportHeight - grid.height;" }],
    expected: "desktop tall 36x80 center: grid is not placed vertically as chosen",
  },
  {
    name: "vertical-offset-outside-scroll-mapping",
    summary: "the offset is placed once at measurement, but the scroll projection drops it",
    case: "history",
    edits: [
      { file: "unified_terminal_page.ts", from: "this.host.style.top = `${target * cellHeight + this.gridOffsetY}px`;", to: "this.host.style.top = `${target * cellHeight}px`;" },
      { file: "unified_terminal_page.ts", from: "    this.viewport.scrollTop = Math.max(0, Math.min(maximumTop, targetTop));\n    this.applyCanonicalScroll();\n", to: "    this.viewport.scrollTop = Math.max(0, Math.min(maximumTop, targetTop));\n    this.applyCanonicalScroll();\n    this.host.style.top = `${Number.parseFloat(this.host.style.top) + this.gridOffsetY}px`;\n" },
    ],
    expected: "grid is not where the projection puts it",
  },
  {
    name: "setting-not-applied-live",
    summary: "a changed preference does not reach an open terminal",
    case: "live",
    edits: [{ file: "unified_terminal_page.ts", from: "    this.applyTerminalPosition(snapshot.preferences.terminalPosition);\n", to: "" }],
    expected: "desktop: the open terminal did not take center",
  },
  {
    name: "select-ignores-position",
    summary: "the frozen Select surface keeps the old top-left placement",
    case: "select",
    edits: [{ file: "unified_terminal_page.ts", from: "    this.selectBody.style.setProperty(\"--persea-select-grid-width\", this.host.style.width);\n", to: "" }],
    expected: "desktop 36x10 top-center: entering Select moved the text",
  },
  {
    name: "select-ignores-band",
    summary: "the frozen Select surface drops Center's band above the grid",
    case: "select",
    edits: [{ file: "unified_terminal_page.ts", from: "    this.selectBody.style.setProperty(\"--persea-select-offset-top\", `${this.gridOffsetY}px`);\n", to: "" }],
    expected: "desktop 36x10 center: entering Select moved the text",
  },
  {
    name: "loading-ignores-position",
    summary: "the replay notice stays centred whatever the choice",
    case: "loading",
    edits: [{ file: "unified_terminal_surface.css", from: ".persea-unified-terminal[data-terminal-position=\"top-center\"] .persea-unified-loading { justify-content: flex-start; }", to: "" }],
    expected: "desktop top-center: replay notice is not top center",
  },
];

function assert(value, message) { if (!value) throw new Error(message); }

async function build(root, mutant) {
  fs.rmSync(root, { recursive: true, force: true });
  fs.mkdirSync(root, { recursive: true });
  fs.cpSync(path.join(UI, "dist"), path.join(root, "dist"), { recursive: true });
  const applied = new Map();
  await esbuild.build({
    absWorkingDir: UI,
    entryPoints: [path.join(UI, "src/app.ts")],
    bundle: true,
    format: "esm",
    platform: "browser",
    minify: true,
    legalComments: "eof",
    outfile: path.join(root, "dist/app.js"),
    logLevel: "silent",
    plugins: mutant ? [{
      name: "terminal-position-mutant",
      setup(build) {
        build.onLoad({ filter: /\.(ts|css)$/ }, (args) => {
          const edits = mutant.edits.filter((edit) => path.basename(args.path) === edit.file && path.dirname(args.path) === path.join(UI, "src"));
          if (edits.length === 0) return undefined;
          let contents = fs.readFileSync(args.path, "utf8");
          for (const edit of edits) {
            const sites = contents.split(edit.from).length - 1;
            assert(sites === 1, `${mutant.name}: ${edit.file} has ${sites} mutation sites`);
            contents = contents.replace(edit.from, () => edit.to);
            applied.set(edit, true);
          }
          return { contents, loader: args.path.endsWith(".css") ? "css" : "ts", resolveDir: path.dirname(args.path) };
        });
      },
    }] : [],
  });
  assert(!mutant || applied.size === mutant.edits.length, `${mutant?.name}: ${applied.size}/${mutant?.edits.length} edits compiled`);
}

function run(root, testCase) {
  const result = spawnSync(process.execPath, [path.join(__dirname, "run_terminal_position_browser.cjs")], {
    cwd: UI, encoding: "utf8", timeout: 300_000,
    env: { ...process.env, PERSEA_POSITION_UI: root, PERSEA_POSITION_ENGINE: ENGINE, PERSEA_POSITION_CASE: testCase, PERSEA_POSITION_SCREENSHOTS: "" },
  });
  const output = `${result.stdout || ""}${result.stderr || ""}`;
  fs.writeFileSync(path.join(root, "runner.log"), output);
  assert(!result.error && !result.signal, `${path.basename(root)}: infrastructure failure ${result.error || result.signal}`);
  return { status: result.status, output };
}

async function main() {
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const results = [];
  const cases = [...new Set(MUTANTS.map((mutant) => mutant.case))];
  const cleanRoot = path.join(EVIDENCE, "clean");
  await build(cleanRoot);
  for (const testCase of cases) {
    const clean = run(cleanRoot, testCase);
    assert(clean.status === 0, `clean build failed ${testCase}: ${clean.output.slice(-2000)}`);
    results.push({ name: `clean-${testCase}`, status: clean.status, verdict: "pass" });
    console.log(`terminal position clean control ${testCase}: PASS`);
  }
  for (const mutant of MUTANTS) {
    const root = path.join(EVIDENCE, mutant.name);
    await build(root, mutant);
    const outcome = run(root, mutant.case);
    const killed = outcome.status === 1 && outcome.output.includes("TERMINAL_POSITION ") && outcome.output.includes(mutant.expected);
    const failure = outcome.output.split("\n").find((line) => line.includes("TERMINAL_POSITION ")) || outcome.output.slice(-400);
    results.push({ name: mutant.name, summary: mutant.summary, case: mutant.case, status: outcome.status, expected: mutant.expected, failure, verdict: killed ? "killed" : "survived" });
    assert(killed, `${mutant.name} was not killed by "${mutant.expected}" (exit ${outcome.status}): ${failure}`);
    console.log(`terminal position mutant killed: ${mutant.name} — ${failure.trim().slice(0, 240)}`);
  }
  fs.writeFileSync(path.join(EVIDENCE, "mutants.json"), `${JSON.stringify(results, null, 2)}\n`);
  console.log(`terminal position mutants ${ENGINE}: ${MUTANTS.length}/${MUTANTS.length} killed, ${cases.length} clean controls passed`);
}

main().catch((error) => {
  console.error(error instanceof Error ? error.stack || error.message : error);
  process.exitCode = 1;
});
