import type { UnifiedTheme } from "./unified_themes";

type Color = number | readonly [number, number, number];
type Style = { foreground?: Color; background?: Color; bold?: boolean; dim?: boolean; italic?: boolean; underline?: boolean; inverse?: boolean; concealed?: boolean; strike?: boolean };
export type PreviewRun = Readonly<{ text: string; style: Readonly<Style> }>;
const SGR = /\x1b\[([0-9;:]{0,96})m/g;

function sgrStyle(style: Style, parameters: string): Style {
  const parts = parameters === "" ? [0] : parameters.split(";").map(part => part.includes(":") ? part : Number(part || "0"));
  const color = (values: readonly number[]): Color | undefined => values.length === 1 && values[0] >= 0 && values[0] <= 255 ? values[0]
    : values.length === 3 && values.every(value => Number.isInteger(value) && value >= 0 && value <= 255) ? [values[0], values[1], values[2]] : undefined;
  for (let i = 0; i < parts.length; i++) {
    const parameter = parts[i];
    if (typeof parameter === "string") {
      const fields = parameter.split(":"); const key = Number(fields[0]); const kind = Number(fields[1]);
      const values = kind === 2 ? fields.slice(fields.length === 6 ? 3 : 2).map(Number) : kind === 5 ? fields.slice(2).map(Number) : [];
      const value = color(values);
      if ((key === 38 || key === 48) && value !== undefined) style[key === 38 ? "foreground" : "background"] = value;
      continue;
    }
    if (parameter === 0) style = {};
    else if (parameter === 1) style.bold = true;
    else if (parameter === 2) style.dim = true;
    else if (parameter === 3) style.italic = true;
    else if (parameter === 4) style.underline = true;
    else if (parameter === 7) style.inverse = true;
    else if (parameter === 8) style.concealed = true;
    else if (parameter === 9) style.strike = true;
    else if (parameter === 22) { delete style.bold; delete style.dim; }
    else if (parameter === 23) delete style.italic;
    else if (parameter === 24) delete style.underline;
    else if (parameter === 27) delete style.inverse;
    else if (parameter === 28) delete style.concealed;
    else if (parameter === 29) delete style.strike;
    else if (parameter === 39) delete style.foreground;
    else if (parameter === 49) delete style.background;
    else if (parameter >= 30 && parameter <= 37) style.foreground = parameter - 30;
    else if (parameter >= 90 && parameter <= 97) style.foreground = parameter - 90 + 8;
    else if (parameter >= 40 && parameter <= 47) style.background = parameter - 40;
    else if (parameter >= 100 && parameter <= 107) style.background = parameter - 100 + 8;
    else if (parameter === 38 || parameter === 48) {
      const mode = parts[++i]; const size = mode === 2 ? 3 : mode === 5 ? 1 : 0;
      if (size === 0) continue;
      const values = parts.slice(i + 1, i + 1 + size);
      const value = values.every(value => typeof value === "number") ? color(values as number[]) : undefined;
      i += size;
      if (value !== undefined) style[parameter === 38 ? "foreground" : "background"] = value;
    }
  }
  return style;
}

/** SGR is interpreted as a small data format, never as HTML or a terminal. */
export function previewRuns(rows: readonly string[]): readonly PreviewRun[] {
  const source = rows.join("\n");
  if (rows.length > 41 || source.length > 8192) throw new Error("Preview exceeds its bounds");
  const runs: PreviewRun[] = []; let style: Style = {}; let offset = 0;
  const append = (text: string): void => {
    if (/\x1b|[\x00-\x08\x0b-\x1f\x7f-\x9f]/.test(text)) throw new Error("Invalid preview styling");
    if (text) runs.push({ text, style: { ...style } });
  };
  for (const match of source.matchAll(SGR)) {
    append(source.slice(offset, match.index));
    style = sgrStyle(style, match[1]); offset = match.index! + match[0].length;
  }
  append(source.slice(offset));
  return runs;
}

function cssColor(color: Color | undefined, palette: UnifiedTheme): string | undefined {
  if (color === undefined) return undefined;
  if (typeof color !== "number") return `rgb(${color.join(",")})`;
  if (color < 16) return palette.ansi[color];
  if (color >= 232) { const gray = 8 + (color - 232) * 10; return `rgb(${gray},${gray},${gray})`; }
  const n = color - 16; const channel = (level: number): number => level === 0 ? 0 : 55 + 40 * level;
  return `rgb(${channel(Math.floor(n / 36))},${channel(Math.floor(n / 6) % 6)},${channel(n % 6)})`;
}

export function renderTerminalPreview(screen: HTMLElement, rows: readonly string[], ansiRows: readonly string[] | undefined, palette: UnifiedTheme): void {
  let runs: readonly PreviewRun[];
  try {
    runs = previewRuns(ansiRows ?? rows);
    if (runs.map(run => run.text).join("") !== rows.join("\n")) throw new Error("Preview representations differ");
  } catch {
    // A malformed styling stream can only lose color. It cannot gain markup,
    // URLs, terminal side effects, or text different from the plain snapshot.
    screen.textContent = rows.join("\n").replace(/[\x00-\x08\x0b-\x1f\x7f-\x9f]/g, ""); return;
  }
  const fragment = document.createDocumentFragment();
  for (const { text, style } of runs) {
    const span = document.createElement("span"); span.textContent = text;
    let foreground = cssColor(style.foreground, palette), background = cssColor(style.background, palette);
    if (style.inverse) [foreground, background] = [background ?? palette.background, foreground ?? palette.foreground];
    if (foreground) span.style.color = foreground;
    if (background) span.style.backgroundColor = background;
    if (style.bold) span.style.fontWeight = "700";
    if (style.dim) span.style.opacity = "0.7";
    if (style.italic) span.style.fontStyle = "italic";
    if (style.underline || style.strike) span.style.textDecoration = [style.underline ? "underline" : "", style.strike ? "line-through" : ""].filter(Boolean).join(" ");
    if (style.concealed) span.style.visibility = "hidden";
    fragment.append(span);
  }
  if (rows.join("").trim() === "") fragment.append(document.createTextNode("(blank pane)"));
  screen.replaceChildren(fragment);
}
