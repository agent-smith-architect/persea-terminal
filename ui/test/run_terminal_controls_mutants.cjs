"use strict";

// Fn-bank-only mutants are superseded by test:terminal-keys-mutants.
// Causal terminal controls gate. A non-zero exit is not evidence by itself: every mutant
// must compile, the clean fixture must pass, and the mutant must terminate in
// the assertion that names the invariant it was designed to violate. Spawn,
// browser, fixture and compilation failures are classified as gate failures.
const fs = require("fs");
const path = require("path");
const { spawnSync } = require("child_process");

const UI = path.resolve(__dirname, "..");
const EVIDENCE = path.resolve(process.env.PERSEA_TERMINAL_CONTROLS_EVIDENCE_DIR || "/tmp/persea-terminal_controls-mutants");
const BROWSER_RUNNER = path.join(__dirname, "run_terminal_controls_browser.cjs");
const BROWSER_MUTANTS = Object.freeze({
  "clipped-status-notice": "status notice clips its explanation",
  "native-key-echo-lost": "native dictation restart duplicated text or sent Enter",
  "f12-keycode": "F-key bytes/events drifted",
  "no-control-gate": "F1 sent INPUT before MODE(CONTROL)",
  "row-drift": "default row drifted",
  "second-row": "key bar has more than one row",
  "typography-refocus": "typography opening changed focus",
  "selection-loss": "typography change lost selection or textarea scroll",
  "typography-input": "composer controls touched terminal wire",
  "typography-resize": "composer controls touched terminal wire",
  "popover-in-flow": "opening typography moved composer boxes",
  "no-cas": "typography PUT/CAS drifted",
  "clamp-enabled": "plus bound is not focusable aria-disabled with guarded activation",
  "conflict-lie": "conflict status/value is not honest",
  "unavailable-lie": "unavailable status/value is not honest",
  "standard-marker": "standard row exposes a transient bank marker",
  "no-typography-claim": "typography did not close View through the shared owner",
  "no-typography-competitor-close": "sheet did not close typography through the shared owner",
  "no-typography-lifecycle-close": "reconnect did not restore exact row/popover lifecycle state",
  "composer-overflow-visible": "composer containment was disabled",
  "theme-border-collapse": "typography semantic contrast/containment failed",
  "theme-status-collapse": "typography semantic contrast/containment failed",
  "popover-no-flip": "typography did not flip below a top anchor",
  "portal-controls-downsize": "portal control under 44px on coarse pointer",
  "theme-weak-palette": "typography semantic contrast/containment failed",
  "typography-native-disable": "typography keyboard focus destination became unusable while Plus saved",
  "typography-stale-frame-restore": "typography deferred restore overwrote newer interaction",
  "sheet-gesture-generation-fence": "overlapping pointer release borrowed another contact's authority",
  "keyboard-activation-generation-fence": "standard Ctrl keyboard activation crossed disclosure generation",
  "keyboard-canceled-tombstone": "canceled keyboard activation was reclassified as no-keydown accessibility activation",
  "standard-key-generation-fence": "standard Ctrl keyboard activation crossed disclosure generation",
  "paste-generation-fence": "Paste keyboard activation crossed disclosure generation",
  "keyboard-enter-repeat-provenance": "held Enter repeat was reclassified as no-keydown accessibility activation",
  "typography-layout-reanchor": "typography portal did not follow composer layout",
  "keyboard-canceled-release-retirement": Object.freeze({
    expected: "released canceled tombstone rejected the next trusted no-keydown accessibility activation",
    engine: "webkit",
  }),
  "toolbar-composer-generation-fence": "toolbar composer keyboard activation crossed disclosure generation",
  "composer-control-generation-fence": "Composer Insert keyboard activation crossed disclosure generation",
  "session-generation-provider-omission": "identity SessionSwitcher keyboard activation crossed disclosure generation",
  "session-sheet-generation-provider-omission": "sheet SessionSwitcher keyboard activation crossed disclosure generation",
  "keyboard-unrelated-key-provenance": "unrelated key erased held keyboard activation provenance",
  "tap-pointer-identity": "overlapping pointer release borrowed another contact's authority",
});
const CORRECTION_REQUIREMENT = Object.freeze({
  "no-typography-claim": "R3", "no-typography-competitor-close": "R3", "no-typography-lifecycle-close": "R3",
  "standard-marker": "R4", "row-drift": "R4",
  "theme-border-collapse": "R5", "theme-status-collapse": "R5",
  "popover-in-flow": "R6", "composer-overflow-visible": "R6", "popover-no-flip": "R6",
  "portal-controls-downsize": "C2-3",
  "theme-weak-palette": "C2-4",
  "typography-native-disable": "R9",
  "typography-stale-frame-restore": "R10",
  "sheet-gesture-generation-fence": "C4",
  "keyboard-activation-generation-fence": "R12",
  "keyboard-canceled-tombstone": "C6",
  "standard-key-generation-fence": "C6",
  "paste-generation-fence": "C6",
  "keyboard-enter-repeat-provenance": "C6",
  "typography-layout-reanchor": "R14",
  "keyboard-canceled-release-retirement": "C7",
  "toolbar-composer-generation-fence": "C7",
  "composer-control-generation-fence": "C7",
  "session-generation-provider-omission": "C7",
  "session-sheet-generation-provider-omission": "C7",
  "keyboard-unrelated-key-provenance": "C7",
  "tap-pointer-identity": "C7",
});
const INFRASTRUCTURE = Object.freeze([
  /mutant (?:did not touch|replaced \d+)/i,
  /Build failed|Transform failed|Could not resolve/i,
  /fixture wait timed out|ECONN(?:REFUSED|RESET)|ERR_CONNECTION/i,
  /browser (?:has disconnected|closed|crash)|Target page, context or browser has been closed/i,
  /Playwright has no|executable doesn't exist|Chrome target unavailable/i,
  /TimeoutError:/i,
]);

function assert(value, message) { if (!value) throw new Error(message); }
function outputOf(result) { return `${result.stdout || ""}${result.stderr || ""}`; }
function spawnBrowser(directory, mutant = "", engine = "chromium") {
  fs.mkdirSync(directory, { recursive: true });
  const result = spawnSync(process.execPath, [BROWSER_RUNNER], {
    cwd: UI,
    env: {
      ...process.env,
      PERSEA_TERMINAL_CONTROLS_ENGINE: engine,
      PERSEA_TERMINAL_CONTROLS_CASE: "phone-390",
      PERSEA_TERMINAL_CONTROLS_MUTANT: mutant,
      PERSEA_TERMINAL_CONTROLS_EVIDENCE_DIR: directory,
    },
    encoding: "utf8",
    timeout: 90_000,
  });
  const output = outputOf(result);
  fs.writeFileSync(path.join(directory, "runner.log"), output);
  return { result, output };
}

function validateProcess(result, output, label) {
  assert(!result.error, `${label}: spawn failed ${String(result.error)}`);
  assert(result.signal === null, `${label}: process died by signal ${result.signal}`);
  const infrastructure = INFRASTRUCTURE.find((pattern) => pattern.test(output));
  assert(!infrastructure, `${label}: infrastructure signature ${infrastructure} in output`);
}

async function compilePreferenceCase(outfile, mutant = "") {
  const esbuild = require(path.join(UI, "node_modules/esbuild"));
  let touched = 0;
  await esbuild.build({
    entryPoints: [path.join(UI, "test/operator_preferences.test.ts")],
    bundle: true,
    platform: "node",
    format: "cjs",
    outfile,
    logLevel: "silent",
    ...(mutant ? { plugins: [{
      name: `terminal_controls-${mutant}`,
      setup(build) {
        build.onLoad({ filter: /operator_preferences\.ts$/ }, (args) => {
          let source = fs.readFileSync(args.path, "utf8");
          let needle;
          let replacement;
          if (mutant === "preview-cleared-by-prior") {
            needle = "    this.authoritative = Object.freeze({ record, status, message });";
            replacement = `${needle}\n    if (this.composerPreview !== undefined) this.composerPreview = undefined;`;
          } else if (mutant === "queued-write-uses-preview") {
            needle = "    const next = this.merge(patch, authority.preferences);";
            replacement = "    const next = this.merge(patch, this.current.preferences);";
          } else {
            throw new Error(`unknown preference mutant ${mutant}`);
          }
          const pieces = source.split(needle);
          assert(pieces.length === 2, `${mutant} replaced ${pieces.length - 1}, expected 1`);
          source = pieces.join(replacement);
          touched += 1;
          return { contents: source, loader: "ts", resolveDir: path.dirname(args.path) };
        });
      },
    }] } : {}),
  });
  assert(!mutant || touched === 1, `${mutant} did not compile a touched source module`);
}

function spawnNode(file, directory) {
  const result = spawnSync(process.execPath, [file], { cwd: UI, encoding: "utf8", timeout: 30_000 });
  const output = outputOf(result);
  fs.writeFileSync(path.join(directory, "runner.log"), output);
  return { result, output };
}

async function main() {
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const results = [];
  const summary = (status, error = null) => fs.writeFileSync(path.join(EVIDENCE, "summary.json"), `${JSON.stringify({ status, error, results }, null, 2)}\n`);
  try {
    const browserControlDir = path.join(EVIDENCE, "control-browser");
    const browserControl = spawnBrowser(browserControlDir);
    validateProcess(browserControl.result, browserControl.output, "browser clean control");
    assert(browserControl.result.status === 0 && browserControl.output.includes("terminal controls chromium: PASS (1 shapes)"), `browser clean control failed with status ${browserControl.result.status}`);
    results.push({ case: "control-browser", control: true, passed: true, status: 0 });

    const preferenceControlDir = path.join(EVIDENCE, "control-preferences");
    fs.mkdirSync(preferenceControlDir, { recursive: true });
    const preferenceControlFile = path.join(preferenceControlDir, "operator-preferences-control.cjs");
    await compilePreferenceCase(preferenceControlFile);
    const preferenceControl = spawnNode(preferenceControlFile, preferenceControlDir);
    validateProcess(preferenceControl.result, preferenceControl.output, "preference clean control");
    assert(preferenceControl.result.status === 0, `preference clean control failed with status ${preferenceControl.result.status}`);
    results.push({ case: "control-preferences", control: true, compiled: true, passed: true, status: 0 });

    const previewDir = path.join(EVIDENCE, "preview-cleared-by-prior");
    fs.mkdirSync(previewDir, { recursive: true });
    const previewFile = path.join(previewDir, "operator-preferences-mutant.cjs");
    await compilePreferenceCase(previewFile, "preview-cleared-by-prior");
    const preview = spawnNode(previewFile, previewDir);
    validateProcess(preview.result, preview.output, "preview-cleared-by-prior");
    const previewExpected = "subscriber A lost the preview to the prior PUT";
    assert(preview.result.status === 1 && preview.output.includes(previewExpected), `preview-cleared-by-prior did not die at intended assertion (status ${preview.result.status})`);
    results.push({ mutant: "preview-cleared-by-prior", requirement: "R1", compiled: true, killed: true, status: 1, expected: previewExpected });
    console.log("terminal controls mutant killed causally: preview-cleared-by-prior");

    const queuedWriteDir = path.join(EVIDENCE, "queued-write-uses-preview");
    fs.mkdirSync(queuedWriteDir, { recursive: true });
    const queuedWriteFile = path.join(queuedWriteDir, "operator-preferences-mutant.cjs");
    await compilePreferenceCase(queuedWriteFile, "queued-write-uses-preview");
    const queuedWrite = spawnNode(queuedWriteFile, queuedWriteDir);
    validateProcess(queuedWrite.result, queuedWrite.output, "queued-write-uses-preview");
    const queuedWriteExpected = "success: B wire body leaked C preview";
    assert(queuedWrite.result.status === 1 && queuedWrite.output.includes(queuedWriteExpected), `queued-write-uses-preview did not die at intended assertion (status ${queuedWrite.result.status})`);
    results.push({ mutant: "queued-write-uses-preview", requirement: "C2-1", compiled: true, killed: true, status: 1, expected: queuedWriteExpected });
    console.log("terminal controls mutant killed causally: queued-write-uses-preview");

    for (const [mutant, specification] of Object.entries(BROWSER_MUTANTS)) {
      const { expected, engine } = typeof specification === "string"
        ? { expected: specification, engine: "chromium" }
        : specification;
      const directory = path.join(EVIDENCE, mutant);
      const run = spawnBrowser(directory, mutant, engine);
      validateProcess(run.result, run.output, mutant);
      assert(run.result.status === 1, `${mutant}: expected assertion exit 1, got ${run.result.status}`);
      assert(run.output.includes("TERMINAL_CONTROLSAssertionError: TERMINAL_CONTROLS_ASSERTION:"), `${mutant}: failure was not a browser assertion`);
      assert(run.output.includes(expected), `${mutant}: missing intended failure signature ${JSON.stringify(expected)}`);
      results.push({ mutant, requirement: CORRECTION_REQUIREMENT[mutant] || "TERMINAL_CONTROLS-baseline", engine, compiled: true, killed: true, status: 1, expected });
      console.log(`terminal controls mutant killed causally: ${mutant}`);
    }
    summary("PASS");
    console.log(`terminal controls causal mutants PASS (${results.length - 2}/${results.length - 2} killed; 2/2 clean controls)`);
  } catch (error) {
    summary("FAIL", String(error instanceof Error ? error.stack || error.message : error));
    throw error;
  }
}

void main().catch((error) => {
  console.error(error instanceof Error ? error.stack || error.message : error);
  process.exitCode = 1;
});
