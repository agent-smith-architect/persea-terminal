// Orders inventory reads against alias saves within one document. Each
// physical inventory read takes a number when it starts, and an alias save
// takes one when its reply arrives. A read with a smaller number started before
// that reply, so it can show the alias only as it was before the save.
let last = 0;

export function nextInventoryReadOrder(): number {
  last += 1;
  return last;
}
