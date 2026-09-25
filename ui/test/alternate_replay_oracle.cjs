"use strict";

// Use the application's installed, patched xterm interpreter without a DOM.
// Buffer reads match the region-scrollback browser oracle; Go supplies real
// committed journal bytes and compares these rows with a private tmux capture.
global.self = global;
const fs = require("fs");
const { Terminal } = require("@xterm/xterm");
const input = JSON.parse(fs.readFileSync(0, "utf8"));
const terminal = new Terminal({ cols: input.columns, rows: input.rows, scrollback: 10000, allowProposedApi: true });
terminal.write(Buffer.from(input.data, "base64"), () => {
  const buffer = terminal.buffer.active;
  const lines = [];
  for (let row = 0; row < buffer.length; row++) lines.push(buffer.getLine(row).translateToString(true));
  process.stdout.write(JSON.stringify({ lines, x: buffer.cursorX, y: buffer.cursorY, type: buffer.type }));
  terminal.dispose();
});
