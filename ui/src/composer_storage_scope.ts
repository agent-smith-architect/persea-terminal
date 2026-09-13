export function validateComposerStorageScope(value: string | null): string | undefined {
  if (value === null || value.length === 0 || value.length > 4_096) return undefined;
  // eslint-disable-next-line no-control-regex
  return /[\u0000-\u001f\u007f]/.test(value) ? undefined : value;
}
