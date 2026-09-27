# Browser attachment transport

The WebSocket subprotocol is `persea-terminal.v3`. This is a hard version
boundary: a page loaded before the upgrade cannot reconnect until reloaded.
The attachment state machine still uses frame version 1. Compression is off.

Every server attachment frame (PREPARE, COMMIT, LIVE, MODE, END) is one WebSocket
binary message:

| Bytes | Meaning |
| --- | --- |
| 0–3 | Unsigned big-endian length of the JSON header, in bytes |
| 4 through 4 + length − 1 | UTF-8 JSON object with the typed attachment fields |
| Remaining bytes | Raw LIVE data or PREPARE replay; empty for other frame types |

The header retains `version`, `type`, `source`, decimal-string `epoch` and
`cut`, and each frame type's existing fields. LIVE's `data` and PREPARE's
`replay` are now integer byte lengths, including zero for an empty replay.
The declared length must equal the entire remaining body. LIVE must be nonempty.
Terminal bytes are never interpreted as text by the codec, including invalid
UTF-8 and fragments of escape or Unicode sequences.

PREPARE retains `kind`, `columns`, `rows`, `history` (an array of strings),
`truncated`, and, for HISTORY cuts, decimal-string `request` and
`effective_history_rows`. All uint64 strings are canonical positive decimals.
Unknown, duplicate, missing, wrong-direction and wrongly typed fields are
rejected. JSON object order and insignificant whitespace are not significant.
Extra JSON values and extra body bytes are rejected.

The header is limited to `6 * 2 MiB + 4096` bytes: worst-case JSON escaping of
the bounded history, plus scalar fields. Its length must also fit the message.
The whole wire message remains bounded at 16 MiB, matching the internal framing
limit. Bounds are checked before decoding the header or allocating payload
copies. Existing typed bounds still apply: 2 MiB history, 10,000 history rows,
8 KiB per row, 256 KiB replay, and 512 KiB LIVE. Ordinary broker output is
chunked at 16 KiB; the larger LIVE bound is the generic protocol ceiling.

Browser attachment requests remain WebSocket text JSON. INPUT uses canonical
padded base64. Liveness, refusal and flow transport messages retain their
reserved text namespaces. Flow ACKs count attachment frames after consumption,
including binary frames with no body; they do not count bytes or text transport
messages.

The broker sends the browser encoding inside `proto.FrameAttachment`. The
front door validates every frame and relays the exact bytes as binary. The page
sets `binaryType = "arraybuffer"` and uses the same envelope and typed checks.
This payload contains no broker cursor or other private relay metadata.

Using one binary form for all server attachment types keeps direction and
message-type validation uniform. Only byte-carrying frames gain a raw body;
there is no second server attachment codec or text fallback.
