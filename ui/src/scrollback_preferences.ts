export const SCROLLBACK_CHOICES = Object.freeze([0, 500, 1_000, 2_000, 5_000, 7_500, 10_000] as const);
export type ScrollbackRows = typeof SCROLLBACK_CHOICES[number];
export const DEFAULT_SCROLLBACK_ROWS: ScrollbackRows = 1_000;
// The shared recording must not lose rows because a smaller device opens it first.
// Only the browser's renderer follows the device/terminal preference below.
export const ADOPTION_HISTORY_ROWS = 10_000;
export const SCROLLBACK_STORAGE_KEY = "persea-terminal.scrollback.v1";
export const TERMINAL_SCROLLBACK_STORAGE_KEY = "persea-terminal.scrollback.sessions.v1";
const MAX_TERMINAL_PREFERENCES = 128;

type TerminalPreference = readonly [scope: string, rows: ScrollbackRows];

function validScope(scope: unknown): scope is string {
  return typeof scope === "string" && scope.length > 0 && scope.length <= 4096;
}

function terminalPreferences(): TerminalPreference[] {
  try {
    const raw: unknown = JSON.parse(window.localStorage.getItem(TERMINAL_SCROLLBACK_STORAGE_KEY) ?? "[]");
    if (!Array.isArray(raw)) return [];
    return raw.slice(-MAX_TERMINAL_PREFERENCES).flatMap((entry: unknown): TerminalPreference[] => {
      if (!Array.isArray(entry) || entry.length !== 2 || !validScope(entry[0])) return [];
      const rows = scrollbackRows(entry[1]);
      return rows === undefined ? [] : [[entry[0], rows]];
    });
  } catch { return []; }
}

export function scrollbackRows(value: unknown): ScrollbackRows | undefined {
  return SCROLLBACK_CHOICES.find(rows => String(rows) === String(value));
}

export function readScrollbackRows(): ScrollbackRows {
  try { return scrollbackRows(window.localStorage.getItem(SCROLLBACK_STORAGE_KEY)) ?? DEFAULT_SCROLLBACK_ROWS; }
  catch { return DEFAULT_SCROLLBACK_ROWS; }
}

export function saveScrollbackRows(rows: ScrollbackRows): boolean {
  try { window.localStorage.setItem(SCROLLBACK_STORAGE_KEY, String(rows)); return true; }
  catch { return false; }
}

export function terminalScrollbackOverride(scope: string | null | undefined): ScrollbackRows | undefined {
  if (!validScope(scope)) return undefined;
  return terminalPreferences().reverse().find(entry => entry[0] === scope)?.[1];
}

export function readTerminalScrollbackRows(scope: string | null | undefined, fallback: ScrollbackRows = readScrollbackRows()): ScrollbackRows {
  return terminalScrollbackOverride(scope) ?? fallback;
}

export function saveTerminalScrollbackRows(scope: string | null | undefined, rows: ScrollbackRows | undefined): boolean {
  if (!validScope(scope)) return false;
  try {
    const entries = terminalPreferences().filter(entry => entry[0] !== scope);
    if (rows !== undefined) entries.push([scope, rows]);
    window.localStorage.setItem(TERMINAL_SCROLLBACK_STORAGE_KEY, JSON.stringify(entries.slice(-MAX_TERMINAL_PREFERENCES)));
    return true;
  } catch { return false; }
}

export function createScrollbackControl(rows: ScrollbackRows, changed: (rows: ScrollbackRows) => void, options: Readonly<{ deviceDefault?: boolean; useDeviceDefault?: () => void }> = {}): { label: HTMLLabelElement; select: HTMLSelectElement } {
  const label = document.createElement("label"); label.className = "scrollback-control";
  const caption = document.createElement("span"); caption.className = "scrollback-control__caption";
  const title = document.createElement("span"); title.textContent = options.deviceDefault ? "Default scrollback" : "Scrollback";
  const scope = document.createElement("small"); scope.textContent = options.deviceDefault ? "This device" : "This terminal · this device";
  caption.append(title, scope);
  const select = document.createElement("select"); select.setAttribute("aria-label", "Scrollback rows");
  if (options.useDeviceDefault) {
    const option = document.createElement("option"); option.value = "default"; option.textContent = `Device default (${readScrollbackRows().toLocaleString()} rows)`;
    select.append(option);
  }
  for (const count of SCROLLBACK_CHOICES) {
    const option = document.createElement("option"); option.value = String(count); option.textContent = count ? `${count.toLocaleString()} rows` : "Screen only";
    select.append(option);
  }
  select.value = String(rows);
  select.addEventListener("change", () => {
    if (select.value === "default") { options.useDeviceDefault?.(); return; }
    const next = scrollbackRows(select.value); if (next !== undefined) changed(next);
  });
  label.append(caption, select); return { label, select };
}
