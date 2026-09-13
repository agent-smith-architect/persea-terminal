"use strict";

const fs = require("node:fs");
const path = require("node:path");
const { gzipSync } = require("node:zlib");
const { buildSync } = require("esbuild");

const development = process.argv.includes("--development");
const output = path.join(__dirname, "dist");
fs.rmSync(output, { recursive: true, force: true });
fs.mkdirSync(output, { recursive: true });
const result = buildSync({
  absWorkingDir: __dirname,
  entryPoints: ["src/app.ts"],
  bundle: true,
  format: "esm",
  platform: "browser",
  minify: !development,
  sourcemap: development,
  legalComments: "eof",
  outfile: "dist/app.js",
  metafile: true,
});
fs.writeFileSync(path.join(output, "app.meta.json"), JSON.stringify(result.metafile));
for (const file of ["index.html", "manifest.webmanifest", "icon-192.png", "icon-512.png", "apple-touch-icon.png"]) {
  fs.copyFileSync(path.join(__dirname, file), path.join(output, file));
}
fs.copyFileSync(require.resolve("@xterm/xterm/css/xterm.css"), path.join(output, "xterm.css"));
fs.copyFileSync(require.resolve("@xterm/xterm/LICENSE"), path.join(output, "THIRD_PARTY_NOTICES.txt"));
// These contain only release assets. HTML receives a per-response nonce and
// session/API data must never enter a shared compressed representation.
for (const file of ["app.js", "app.css", "xterm.css"]) {
  fs.writeFileSync(path.join(output, `${file}.gz`), gzipSync(fs.readFileSync(path.join(output, file))));
}
