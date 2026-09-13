"use strict";

// Trace npm chains and Node/Go harness runners. This checks wiring; CI must
// still execute the resulting tests rather than treating references as proof.
const fs = require("node:fs");
const path = require("node:path");
const UI = path.resolve(__dirname, "..");
const root = path.dirname(UI);
const scripts = JSON.parse(fs.readFileSync(path.join(UI, "package.json"), "utf8")).scripts;
const workflow = fs.readFileSync(path.join(root, ".github/workflows/ci.yml"), "utf8");
const failures = [];
const visited = new Set();
const source = [];
const stripComments = (text) => text.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*(?:\/\/|#).*$/gm, "");

function scriptNames(text) {
  return Array.from(text.matchAll(/\bnpm (?:run ([\w:-]+)|(test))\b/g), (match) => match[1] || match[2]);
}

function visitScript(name) {
  if (visited.has(`script:${name}`)) return;
  visited.add(`script:${name}`);
  const command = scripts[name];
  if (command === undefined) { failures.push(`missing npm script: ${name}`); return; }
  scan(command);
}

function visitFile(file) {
  if (visited.has(file)) return;
  visited.add(file);
  scan(stripComments(fs.readFileSync(file, "utf8")));
}

function scan(text) {
  source.push(text);
  for (const name of scriptNames(text)) visitScript(name);
  for (const match of text.matchAll(/(?:test\/)?([\w.-]+\.(?:cjs|ts))/g)) {
    const file = path.join(UI, "test", match[1]);
    if (fs.existsSync(file)) visitFile(file);
  }
}

for (const name of scriptNames(stripComments(workflow))) visitScript(name);
// CI may run browser groups in parallel, but it must cover the same groups
// as the local aggregate command, along with the baseline contracts.
// These causal suites remain mandatory even when their containing groups move.
const requiredBrowserLanes = [
  "test:loading-state-browser", "test:ux10-browser", "test:ux11-browser",
  "test:ux12-refit-accounting-browser",
];
for (const name of ["test", ...scriptNames(scripts["test:browser"] || ""), ...requiredBrowserLanes]) {
  if (!visited.has(`script:${name}`)) failures.push(`test group is unreachable from CI: ${name}`);
}
const harnesses = fs.readdirSync(__dirname).filter((name) => name.endsWith("_browser_harness.ts"));
if (harnesses.length === 0) failures.push("no browser harnesses found");
for (const name of harnesses) {
  if (!source.some((text) => text.includes(name))) failures.push(`browser harness is unreachable from CI: ${name}`);
}
if (failures.length) {
  console.error(`CI reachability check failed:\n${failures.map((failure) => `  - ${failure}`).join("\n")}`);
  process.exitCode = 1;
} else {
  console.log(`CI reachability: ${harnesses.length} browser harnesses reachable through the workflow's entrypoints`);
}
