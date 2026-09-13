import { Terminal } from "@xterm/xterm";
import { KEY_GROUPS, planKey } from "../src/terminal_actions";
import { syntheticCtrlReleaseEvent, syntheticKeydownEvent, unifiedKeyDescriptor } from "../src/unified_key_bar";

const named: Record<string, string> = { escape: "\x1b", tab: "\t", enter: "\r", backspace: "\x7f", insert: "\x1b[2~", delete: "\x1b[3~", "page-up": "\x1b[5~", "page-down": "\x1b[6~" };
const navigation: Record<string, string> = { "arrow-up": "A", "arrow-down": "B", "arrow-left": "D", "arrow-right": "C", home: "H", end: "F" };
const functions = ["P", "Q", "R", "S", "15", "17", "18", "19", "20", "21", "23", "24"];
function expected(key: string, ctrl: boolean, alt: boolean, shift: boolean, application: boolean): string {
  const modifier = 1 + (shift ? 1 : 0) + (alt ? 2 : 0) + (ctrl ? 4 : 0);
  if (key.length === 1) {
    const i = "`1234567890-=[]\\;',./".indexOf(key);
    let c = shift ? /^[a-z]$/.test(key) ? key.toUpperCase() : i >= 0 ? "~!@#$%^&*()_+{}|:\"<>?"[i] : key : key;
    if (ctrl) {
      if (/^[a-z]$/i.test(c)) c = String.fromCharCode(c.toLowerCase().charCodeAt(0) - 96);
      else c = ({ " ": "\0", "@": "\0", "2": "\0", "[": "\x1b", "\\": "\x1c", "]": "\x1d", "^": "\x1e", "_": "\x1f", "3": "\x1b", "4": "\x1c", "5": "\x1d", "6": "\x1e", "7": "\x1f", "8": "\x7f" } as Record<string, string>)[c];
    }
    return (alt ? "\x1b" : "") + c;
  }
  if (navigation[key]) return `\x1b${modifier > 1 ? `[1;${modifier}` : application ? "O" : "["}${navigation[key]}`;
  if (/^f\d+$/.test(key)) {
    const index = Number(key.slice(1)) - 1; const code = functions[index];
    return index < 4 ? `\x1b${modifier > 1 ? `[1;${modifier}` : "O"}${code}` : `\x1b[${code}${modifier > 1 ? `;${modifier}` : ""}~`;
  }
  if (key === "delete" && modifier > 1) return `\x1b[3;${modifier}~`;
  if (key.startsWith("page-") && ctrl) return `\x1b[${key === "page-up" ? 5 : 6};${modifier}~`;
  if (key === "tab" && shift) return (alt ? "\x1b" : "") + "\x1b[Z";
  if (key === "backspace") return (alt ? "\x1b" : "") + (ctrl ? "\b" : "\x7f");
  return (alt ? "\x1b" : "") + named[key];
}
(window as unknown as { runTerminalKeyCatalog(): Promise<number> }).runTerminalKeyCatalog = async () => {
  const host = document.createElement("div"); document.body.append(host);
  const terminal = new Terminal({ allowProposedApi: true }); terminal.open(host);
  let emitted = "";
  terminal.onData((data) => { emitted += data; });
  let count = 0;
  try {
    for (const application of [false, true]) {
      await new Promise<void>((resolve) => terminal.write(application ? "\x1b[?1h" : "\x1b[?1l", resolve));
      for (const keys of Object.values(KEY_GROUPS)) for (const key of keys) for (let mask = 0; mask < 8; mask++) {
        const modifiers = { ctrl: !!(mask & 1), alt: !!(mask & 2), shift: !!(mask & 4) };
        const plan = planKey({ key, modifiers });
        if (typeof plan === "string") continue;
        emitted = "";
        if (plan.prefixEscape) { terminal.textarea!.dispatchEvent(syntheticKeydownEvent(unifiedKeyDescriptor("escape")!)); terminal.textarea!.dispatchEvent(syntheticCtrlReleaseEvent()); }
        if (plan.character !== undefined) terminal.textarea!.dispatchEvent(new InputEvent("input", { data: plan.character, inputType: "insertText", bubbles: true }));
        else terminal.textarea!.dispatchEvent(syntheticKeydownEvent(plan.descriptor!));
        terminal.textarea!.dispatchEvent(syntheticCtrlReleaseEvent());
        const want = expected(key, modifiers.ctrl, modifiers.alt, modifiers.shift, application);
        if (emitted !== want) throw new Error(`Catalog ${key} mask=${mask} application=${application}: ${JSON.stringify(emitted)} != ${JSON.stringify(want)}`);
        count++;
      }
    }
    return count;
  } finally { terminal.dispose(); host.remove(); }
};
