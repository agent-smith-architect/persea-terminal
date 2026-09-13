import { unifiedGeometryAvailability, type UnifiedGeometryAvailabilityInput } from "../src/unified_terminal_page";

function assert(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

const ready: UnifiedGeometryAvailabilityInput = Object.freeze({
  action: "fit_rows",
  closed: false,
  prepared: true,
  committed: true,
  controlGranted: true,
  replaying: false,
  capabilityMode: "control",
  fitPending: false,
  refitPending: false,
  committedColumns: 80,
  committedRows: 24,
  keyboardGuard: false,
  bufferType: "normal",
  measurement: 30,
});

const expectCode = (code: string, patch: Partial<UnifiedGeometryAvailabilityInput>): void => {
  const result = unifiedGeometryAvailability({ ...ready, ...patch });
  assert(!result.enabled, `${code}: unexpectedly enabled`);
  assert(result.code === code, `${code}: got ${result.code}`);
  assert(result.message.length > 0 && !/[\r\n]/.test(result.message), `${code}: reason is not bounded display copy`);
};

expectCode("closed", { closed: true });
expectCode("connecting", { prepared: false });
expectCode("connecting", { committed: false });
expectCode("observe_mode", { capabilityMode: "observe" });
expectCode("no_control", { controlGranted: false });
expectCode("replaying", { replaying: true });
expectCode("pending_size", { fitPending: true });
expectCode("pending_size", { refitPending: true });
expectCode("unknown_geometry", { committedColumns: 0 });
expectCode("keyboard", { keyboardGuard: true });
expectCode("missing_measurement", { measurement: undefined });
expectCode("already_fit", { measurement: 24 });
expectCode("alternate_buffer", { action: "fit_width", bufferType: "alternate", measurement: 90 });

for (const action of ["fit_rows", "fit_width", "apply"] as const) {
  const input = { ...ready, action, measurement: action === "apply" ? undefined : action === "fit_rows" ? 30 : 90 };
  const result = unifiedGeometryAvailability(input);
  assert(result.enabled && result.code === "available" && result.message === "", `${action}: ready state not enabled`);
}

// Apply is an explicit typed request: it is not a viewport measurement and
// therefore ignores the keyboard. A typed width change asks the width model
// separately and remains refused on the alternate screen.
assert(unifiedGeometryAvailability({ ...ready, action: "apply", keyboardGuard: true, measurement: undefined }).enabled,
  "Apply inherited the measured-fit keyboard guard");
assert(unifiedGeometryAvailability({ ...ready, action: "fit_width", bufferType: "alternate", measurement: 90 }).code === "alternate_buffer",
  "width did not retain the alternate-buffer refusal");

console.log("geometry availability contract: PASS");
