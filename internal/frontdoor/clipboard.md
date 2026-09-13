# Shared clipboard storage

The authenticated operator shares clipboard items across devices. Neither storage
API sends input to a terminal or stages a file in a realm. The browser must obtain
an explicit selection before using the existing input or image staging APIs.

## Text retention

`/api/snippets` returns each item's `retention_seconds`, `updated_at`, revision,
and nullable `expires_at`. Retention choices are 1800, 14400, 86400, 604800,
2592000 seconds, or 0 for no expiry. Legacy snippets remain permanent. New
captures use the shared default unless an explicit duration is supplied.

Exact body bytes identify duplicates. Repeated capture keeps the existing ID,
renews its revision and update time, and preserves the longest policy and
deadline. PATCH with a body saves the item and renews expiry, even when the body
is unchanged. Editing into an existing body merges into that record atomically;
the source revision still gates the operation. Reading, copying, and pasting do
not renew an item. PATCH with explicit `retention_seconds` and the current
revision replaces the policy and sets the deadline to update time plus that
duration, or null for 0. It can shorten retention and change a permanent item
back to timed retention. Repeating a selection restarts the duration; it never
adds the old remaining time. Update timestamps remain monotonic if the server
clock moves backwards. Body-only saves and duplicate captures still preserve
longer policies and deadlines. Extended and permanent records are protected from automatic eviction;
a full protected store refuses a new distinct item.

OSC 52 PUT retains its private fixed-ID publication revision and CAS boundary.
GET exposes ordinary editable canonical items instead of the authority record.
Publication and canonical content persist together; a duplicate uses its existing
canonical ID. Version 1 text stores migrate to version 2 and preserve the old OSC
content once. The private copy cannot outlive its canonical item: deletion or
replacement of the body's bytes removes the obsolete copy, and a shorter
explicit expiry caps both copies. Deleting the fixed OSC ID also removes its
canonical item. Load repairs orphaned or overlong private copies without
resurrecting content. Stale OSC PUT responses contain only `id` and `revision`,
never clipboard content; an absent or expired publication has revision 0.

`GET /api/clipboard/preferences` returns version 1, `default_retention_seconds`,
and revision with an ETag. PUT accepts only the duration and current revision,
requires CSRF, and refuses a stale revision with 412. Preferences persist in
`clipboard-preferences.json` beside the snippets file. The initial default is
1800 seconds. Missing or malformed persisted fields fail closed.

## Image API

All routes use the existing AF_UNIX ingress identity, single configured operator,
Origin/fetch-site rules, and shared request limiter. POST, PATCH, and DELETE require the
existing CSRF cookie/header. Queries are refused. Responses are `no-store`; raw
images also have `X-Content-Type-Options: nosniff`.

| Route | Request | Response |
| --- | --- | --- |
| `GET /api/clipboard/images` | No query | `200 {"items":[...]}`, most recently updated first |
| `POST /api/clipboard/images` | Raw image body and Content-Type; optional `X-Persea-Clipboard-Origin` and `X-Persea-Clipboard-Retention` | `201` metadata record, including duplicate renewal |
| `GET /api/clipboard/images/{id}` | A 32-character lowercase hexadecimal ID | `200` original image bytes with their validated media type |
| `PATCH /api/clipboard/images/{id}` | Exact JSON with `retention_seconds` and `revision`, CSRF | Updated metadata; 412 for stale revision |
| `DELETE /api/clipboard/images/{id}` | Exact `application/json`, body `{}` or a positive `revision`, CSRF | `204`; supplied revision gates deletion and may return 412 |

Metadata contains `id`, `media_type`, `byte_size`, `created_at`, `updated_at`,
`expires_at`, `retention_seconds`, `revision`, and `origin`. Times use RFC3339 UTC;
`expires_at` is null for permanent retention. Origin is presentation text, trimmed,
valid UTF-8, at most 32 runes, without control characters. It never determines
identity, routing, or a filesystem path.

PNG, JPEG, GIF, and WebP use the existing staging media sniff and dimension
validation, with declared/sniffed agreement. This retains staging's existing
WebP sniff-only policy; images are not re-encoded. The per-image body cap is
`image_upload_max_bytes` (maximum 10 MiB). The store accepts at most 20 images
and 64 MiB of total image payload, plus bounded per-record metadata. Capacity
returns 507; it never evicts a live image or a saved text record. Exact validated
image bytes deduplicate before the capacity check, so renewing an existing image
still works at capacity. Duplicate uploads preserve longer retention. Explicit
PATCH replaces retention and the deadline using the same rule as text. Reads
do not renew it.

Fixed errors: 400 malformed query/origin/delete request; 403 ingress/auth/CSRF;
404 malformed, absent, or expired ID; 405 method; 412 stale revision; 413 body cap; 415 invalid or
mismatched media; 429 shared limiter; 503 disabled/unavailable store; 507 capacity.
Error responses do not echo image bodies, origins, or filenames.

## Persistence and reclamation

The store lives in `clipboard-images/` beside the configured snippets file. The
parent and image directory must be real owner-only 0700 directories. Image files
are owner-only 0600 regular files with one link and random IDs as their names;
the API accepts no filesystem paths. Operations pin the directory with `os.Root`
and refuse symlinks, unsafe permissions, wrong owners, hard links, unknown files,
malformed metadata, inconsistent byte lengths, and out-of-bounds state.

New or renewed records use `PCI2`: a big-endian 32-bit metadata length,
at most 1024 bytes of JSON metadata, then the original image payload. A 0600
exclusive temp file, file fsync, atomic rename, and directory fsync publish the
record using the existing durable-file substrate. Metadata and image bytes are
published together. A post-publication sync failure faults the store; restart
validates the complete record. An interrupted pre-publication temp file is
reclaimed on startup. Existing `PCI1` files are normalized in memory with policy,
update time, and revision; unchanged files remain PCI1 until a renewal or merge
publishes PCI2. Revision overflow refuses the affected merge before publication;
earlier startup cleanup or other completed migrations are not rolled back.

Startup reclaims expired files. Request checks reject content at its exact
deadline. `Run` owns a one-minute maintenance ticker, cancelled and joined during
shutdown, that physically removes expired image files and rewrites the text
store without expired bodies even when no browser is connected. Reclamation
errors emit only a fixed store/error event and retry on later ticks unless the
durable store has faulted. If uploads are disabled later, an existing image store
still opens for maintenance while the API returns 503. Older binaries do not
understand the new persisted formats; retain the prior data backup when rolling
back across this migration.

Run the frontdoor and clipboard race tests with the documented short `TMPDIR`:
`TMPDIR=/tmp go test ./internal/frontdoor` and
`TMPDIR=/tmp go test -race ./internal/frontdoor -run 'TestClipboard|TestSnippet'`.
