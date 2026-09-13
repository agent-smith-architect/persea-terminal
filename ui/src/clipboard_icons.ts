export type ClipboardIcon = "clipboard" | "paste" | "copy" | "trash" | "text" | "image" | "settings" | "back" | "close" | "chevron" | "download" | "check" | "warning" | "info";

const paths: Readonly<Record<ClipboardIcon, readonly string[]>> = {
  clipboard: ["M9 5H6a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7a2 2 0 0 0-2-2h-3", "M9 3h6v4H9z", "M8 12h8M8 16h6"],
  paste: ["M12 3v11m-4-4 4 4 4-4", "M5 13v7h14v-7"],
  copy: ["M9 9h11v11H9z", "M15 5V3H3v12h2"],
  trash: ["M3 6h18M9 6V3h6v3", "m5 6 1 15h12l1-15", "M10 10v7M14 10v7"],
  text: ["M3 5h12M9 5v14M5 19h8", "M19 11v8M15 15h8"],
  image: ["M3 3h18v18H3z", "m3 17 5-5 4 4 3-3 6 6", "M15 7h.01"],
  settings: ["M4 6h16M4 12h16M4 18h16", "M8 3v6M16 9v6M10 15v6"],
  back: ["m15 5-7 7 7 7"],
  close: ["m6 6 12 12M6 18 18 6"],
  chevron: ["m9 5 7 7-7 7"],
  download: ["M12 3v12m-5-5 5 5 5-5", "M4 17v4h16v-4"],
  check: ["M20 6 9 17l-5-5"],
  warning: ["M12 3 2 21h20z", "M12 9v5M12 17h.01"],
  info: ["M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18", "M12 11v6M12 7h.01"],
};

export function clipboardIcon(name: ClipboardIcon): SVGSVGElement {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  for (const [key, value] of Object.entries({ viewBox: "0 0 24 24", width: "20", height: "20", fill: "none", stroke: "currentColor", "stroke-width": "1.7", "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true", focusable: "false" })) svg.setAttribute(key, value);
  for (const d of paths[name]) {
    const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
    path.setAttribute("d", d); svg.append(path);
  }
  return svg;
}
