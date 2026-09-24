import type { ITheme } from "@xterm/xterm";

export const UNIFIED_THEME_IDS = Object.freeze([
  "default",
  "rose-pine",
  "rose-pine-dawn",
  "solarized-dark",
  "gruvbox-dark",
  "one-dark",
  "dracula",
  "catppuccin-mocha",
] as const);

export type UnifiedThemeID = typeof UNIFIED_THEME_IDS[number];
export const DEFAULT_UNIFIED_THEME: UnifiedThemeID = "default";

export type UnifiedTheme = Readonly<{
  id: UnifiedThemeID;
  label: string;
  background: string;
  foreground: string;
  selectionBackground: string;
  selectionForeground: string;
  pageBackground: string;
  chromeBackground: string;
  chromeForeground: string;
  accent: string;
  ansi: readonly string[];
  xterm: Readonly<ITheme>;
}>;

const ANSI_KEYS = Object.freeze([
  "black", "red", "green", "yellow", "blue", "magenta", "cyan", "white",
  "brightBlack", "brightRed", "brightGreen", "brightYellow", "brightBlue", "brightMagenta", "brightCyan", "brightWhite",
] as const);

type ThemeInput = Omit<UnifiedTheme, "xterm">;

function define(input: ThemeInput): UnifiedTheme {
  if (input.ansi.length !== ANSI_KEYS.length) throw new Error(`theme ${input.id} must define sixteen ANSI colours`);
  const xterm: Record<string, string> = {
    background: input.background,
    foreground: input.foreground,
    cursor: input.accent,
    cursorAccent: input.background,
    selectionBackground: input.selectionBackground,
    selectionForeground: input.selectionForeground,
  };
  ANSI_KEYS.forEach((key, index) => { xterm[key] = input.ansi[index]!; });
  return Object.freeze({ ...input, ansi: Object.freeze([...input.ansi]), xterm: Object.freeze(xterm) });
}

// The colour values are the projects' published base palettes, reduced to the
// fixed xterm/page token set preferences owns. They are data rather than CSS generated
// at runtime: adding a palette necessarily passes the data-driven contrast and
// complete-ANSI tests, and CSP never needs an inline style exception.
const THEMES: Readonly<Record<UnifiedThemeID, UnifiedTheme>> = Object.freeze({
  default: define({
    id: "default", label: "Persea dark", background: "#111318", foreground: "#f3f4f6",
    selectionBackground: "#46658f", selectionForeground: "#ffffff",
    pageBackground: "#0b0f14", chromeBackground: "#1b1e25", chromeForeground: "#f3f4f6", accent: "#8fc7ff",
    ansi: ["#20242c", "#f07178", "#8ccf7e", "#e2b86b", "#719cd6", "#c490d1", "#63c5da", "#c7ccd6", "#5b6475", "#ff8b92", "#a6e3a1", "#f7cf85", "#8fb8ee", "#d8a7e4", "#80d9ea", "#ffffff"],
  }),
  "rose-pine": define({
    id: "rose-pine", label: "Rosé Pine", background: "#191724", foreground: "#e0def4",
    selectionBackground: "#524f67", selectionForeground: "#ffffff",
    pageBackground: "#12101c", chromeBackground: "#26233a", chromeForeground: "#e0def4", accent: "#c4a7e7",
    ansi: ["#26233a", "#eb6f92", "#9ccfd8", "#f6c177", "#31748f", "#c4a7e7", "#ebbcba", "#e0def4", "#6e6a86", "#f083a2", "#b1d8df", "#f9cb8b", "#4b8aa5", "#d1b3ef", "#f0c9c7", "#f5f3ff"],
  }),
  "rose-pine-dawn": define({
    id: "rose-pine-dawn", label: "Rosé Pine Dawn", background: "#faf4ed", foreground: "#575279",
    selectionBackground: "#907aa9", selectionForeground: "#191724",
    pageBackground: "#f2e9e1", chromeBackground: "#fffaf3", chromeForeground: "#575279", accent: "#286983",
    ansi: ["#575279", "#b4637a", "#56949f", "#ea9d34", "#286983", "#907aa9", "#d7827e", "#e5e0d8", "#6e6a86", "#c8788d", "#65a5ae", "#f2ac4b", "#3a7e98", "#a08ab8", "#e2918d", "#fffaf3"],
  }),
  "solarized-dark": define({
    id: "solarized-dark", label: "Solarized Dark", background: "#002b36", foreground: "#eee8d5",
    selectionBackground: "#586e75", selectionForeground: "#ffffff",
    pageBackground: "#001f27", chromeBackground: "#073642", chromeForeground: "#eee8d5", accent: "#2aa198",
    ansi: ["#073642", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198", "#eee8d5", "#657b83", "#f24b47", "#9caf00", "#d3a400", "#4aa3e5", "#e5579a", "#45b8ae", "#fdf6e3"],
  }),
  "gruvbox-dark": define({
    id: "gruvbox-dark", label: "Gruvbox Dark", background: "#282828", foreground: "#ebdbb2",
    selectionBackground: "#665c54", selectionForeground: "#ffffff",
    pageBackground: "#1d2021", chromeBackground: "#3c3836", chromeForeground: "#ebdbb2", accent: "#fabd2f",
    ansi: ["#282828", "#cc241d", "#98971a", "#d79921", "#458588", "#b16286", "#689d6a", "#a89984", "#665c54", "#fb4934", "#b8bb26", "#fabd2f", "#83a598", "#d3869b", "#8ec07c", "#fbf1c7"],
  }),
  "one-dark": define({
    id: "one-dark", label: "One Dark", background: "#282c34", foreground: "#e6e6e6",
    selectionBackground: "#4b5263", selectionForeground: "#ffffff",
    pageBackground: "#21252b", chromeBackground: "#2f343f", chromeForeground: "#e6e6e6", accent: "#61afef",
    ansi: ["#282c34", "#e06c75", "#98c379", "#e5c07b", "#61afef", "#c678dd", "#56b6c2", "#abb2bf", "#5c6370", "#ef7d86", "#a9d48a", "#f2ce89", "#74baf2", "#d58be8", "#69c5d0", "#f1f3f5"],
  }),
  dracula: define({
    id: "dracula", label: "Dracula", background: "#282a36", foreground: "#f8f8f2",
    selectionBackground: "#6272a4", selectionForeground: "#ffffff",
    pageBackground: "#1f2029", chromeBackground: "#343746", chromeForeground: "#f8f8f2", accent: "#bd93f9",
    ansi: ["#21222c", "#ff5555", "#50fa7b", "#f1fa8c", "#6272a4", "#bd93f9", "#8be9fd", "#f8f8f2", "#54576a", "#ff6e6e", "#69ff94", "#ffffa5", "#7b8bc1", "#d6acff", "#a4ffff", "#ffffff"],
  }),
  "catppuccin-mocha": define({
    id: "catppuccin-mocha", label: "Catppuccin Mocha", background: "#1e1e2e", foreground: "#cdd6f4",
    selectionBackground: "#585b70", selectionForeground: "#ffffff",
    pageBackground: "#181825", chromeBackground: "#313244", chromeForeground: "#cdd6f4", accent: "#cba6f7",
    ansi: ["#45475a", "#f38ba8", "#a6e3a1", "#f9e2af", "#89b4fa", "#f5c2e7", "#94e2d5", "#bac2de", "#585b70", "#f5a3b8", "#b8edb4", "#fbe8bd", "#9bc0fb", "#f7d0eb", "#a6e9de", "#ffffff"],
  }),
});

export function isUnifiedThemeID(value: unknown): value is UnifiedThemeID {
  return typeof value === "string" && Object.prototype.hasOwnProperty.call(THEMES, value);
}

export function unifiedTheme(id: UnifiedThemeID): UnifiedTheme {
  const theme = THEMES[id];
  if (theme === undefined) throw new Error("unknown unified theme");
  return theme;
}

function channel(value: number): number {
  const normalized = value / 255;
  return normalized <= 0.04045 ? normalized / 12.92 : ((normalized + 0.055) / 1.055) ** 2.4;
}

function luminance(hex: string): number {
  if (!/^#[0-9a-f]{6}$/i.test(hex)) throw new Error("theme colours must be six-digit hex values");
  const value = Number.parseInt(hex.slice(1), 16);
  const red = channel((value >> 16) & 0xff);
  const green = channel((value >> 8) & 0xff);
  const blue = channel(value & 0xff);
  return 0.2126 * red + 0.7152 * green + 0.0722 * blue;
}

export function contrastRatio(first: string, second: string): number {
  const [light, dark] = [luminance(first), luminance(second)].sort((a, b) => b - a);
  return (light! + 0.05) / (dark! + 0.05);
}
