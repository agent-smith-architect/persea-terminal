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

## Resume

A journal generation's committed output is one byte stream; geometry records
sit between its bytes. Each new generation gets a random 26-character base32
`stream` name, which an admission PREPARE (INITIAL or RECONNECT) carries.
Admission PREPARE, geometry PREPARE and LIVE carry `seq` and `offset`, canonical
decimal strings from `0` through 2^63 − 1, sent together: the position after
the frame's last byte. `offset` counts stream bytes; `seq` is the last record
the frame completed, so a frame that ends inside an output record names the
record before it. A geometry keeps the offset and advances the sequence.

A page that has parsed every frame it received offers its position with the
subprotocol `persea-resume.<stream>.<seq>.<offset>`. When the position is in
the session's current stream, the broker admits only the committed output after
it, at the geometry in force there, and the PREPARE carries `resumed: true`;
COMMIT still follows the whole of it. Any other offer gets the whole stream.
A page accepts a resumed admission only for the exact position it offered
with nothing queued to its terminal, and otherwise ends the attachment and
offers nothing next time. A page that claims its own control lease back (the
server had not yet noticed its lost connection) offers its position too;
session switches and history reloads never do.

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
