// The clipboard browser suite excuses only the diagnostics it provokes. A
// deliberate image outage must never excuse an unrelated image failure.
const {expectedClipboardDiagnostics, DISCONNECT_PHASE, OUTAGE_PHASE} = require("./clipboard_diagnostics.cjs") as {
  expectedClipboardDiagnostics(pages: Page[]): Message[];
  DISCONNECT_PHASE: string;
  OUTAGE_PHASE: string;
};

type Message = {phase: string; type?: string; text: string; url?: string};
type Page = {console: Message[]; pageerrors: Message[]; outageResponses: {url: string; phase: string}[]};

function check(condition: boolean, message: string): void {
  if (!condition) throw new Error(`clipboard diagnostics: ${message}`);
}

const origin = "http://127.0.0.1:1";
const images = `${origin}/api/clipboard/images`;
const failed = (phase: string, url: string): Message => ({
  phase,
  type: "error",
  text: "Failed to load resource: the server responded with a status of 503 (Service Unavailable)",
  url,
});
const unexpected = (page: Page): Message[] => {
  const expected = expectedClipboardDiagnostics([page]);
  return [...page.console, ...page.pageerrors].filter(message => !expected.includes(message));
};

{
  // An unmarked failure before the outage must not borrow the outage's mark.
  const early = failed("healthy image refresh", images);
  const marked = failed(OUTAGE_PHASE, images);
  const left = unexpected({console: [early, marked], pageerrors: [], outageResponses: [{url: images, phase: OUTAGE_PHASE}]});
  check(left.length === 1, "one marked image outage excused two image failures");
}
{
  // A marked image failure in its own phase is excused only by its mark.
  const marked = failed(OUTAGE_PHASE, images);
  check(unexpected({console: [marked], pageerrors: [], outageResponses: [{url: images, phase: OUTAGE_PHASE}]}).length === 0, "marked image outage was not excused");
  check(unexpected({console: [marked], pageerrors: [], outageResponses: []}).length === 1, "unmarked image failure was excused by its phase");
}
{
  // WebKit can report the marked failure after the phase has advanced.
  const late = failed("a later phase", `${images}/${"0".repeat(32)}`);
  check(unexpected({console: [late], pageerrors: [], outageResponses: [{url: late.url as string, phase: OUTAGE_PHASE}]}).length === 0, "late marked image outage was not excused");
}
{
  // Snippet outages are unmarked and excused only during the outage phase.
  const snippets = `${origin}/api/snippets`;
  check(unexpected({console: [failed(OUTAGE_PHASE, snippets)], pageerrors: [], outageResponses: []}).length === 0, "snippet outage was not excused");
  check(unexpected({console: [failed("healthy image refresh", snippets)], pageerrors: [], outageResponses: []}).length === 1, "snippet failure outside the outage was excused");
}
{
  // The deliberate disconnect may report its WebSocket refusal.
  const refusal: Message = {phase: DISCONNECT_PHASE, text: "WebSocket connection failed: Error during WebSocket handshake: Unexpected response code: 410"};
  check(unexpected({console: [refusal], pageerrors: [], outageResponses: []}).length === 0, "disconnect refusal was not excused");
  check(unexpected({console: [{...refusal, phase: OUTAGE_PHASE}], pageerrors: [], outageResponses: []}).length === 1, "WebSocket refusal outside the disconnect was excused");
}
console.log("clipboard diagnostics: PASS");

export {};
