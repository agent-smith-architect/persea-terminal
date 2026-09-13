// UX15-R1 (): the viewport meta must never cap page zoom. `maximum-scale`
// below 2 or `user-scalable=no` defeats WCAG 2.2 SC 1.4.4 (200% resize) on
// every user agent that honours them; iOS focus zoom is prevented at the
// composer instead (a 16px face for the instant of focus on coarse pointers).
const fs = require("node:fs") as { readFileSync(path: string, encoding: "utf8"): string };
const path = require("node:path") as { join(...parts: string[]): string };

function check(condition: boolean, message: string): void {
  if (!condition) throw new Error(`viewport meta: ${message}`);
}

const html = fs.readFileSync(path.join(process.cwd(), "index.html"), "utf8");
const meta = html.match(/<meta name="viewport" content="([^"]+)"/);
check(meta !== null, "index.html carries no viewport meta");
const directives = new Map<string, string>();
for (const entry of (meta as RegExpMatchArray)[1].split(",")) {
  const [key, value] = entry.trim().split("=");
  directives.set(key, value ?? "");
}
check(directives.get("width") === "device-width", "width is not device-width");
check(directives.get("initial-scale") === "1", "initial-scale is not 1");
check(directives.get("user-scalable") !== "no" && directives.get("user-scalable") !== "0", "user-scalable caps page zoom");
const maximum = directives.get("maximum-scale");
check(maximum === undefined || Number(maximum) >= 2, `maximum-scale=${maximum} caps page zoom below 200%`);
console.log("viewport meta: PASS (no zoom cap)");

export {};
