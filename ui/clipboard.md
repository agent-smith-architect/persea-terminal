# Shared Clipboard behavior

Dashboard and the terminal expose the same Clipboard dialog. The clipboard icon
opens it when no terminal text is selected; a selected range still turns that
control into Copy. The terminal menu opens Clipboard, Keys, Composer, image
attachment, session controls, and settings without duplicating raw key buttons.

Action feedback floats below the dialog header without changing the list's
bounds, scroll height or scroll position. A short fade and 4px translation
smooth its appearance and dismissal; reduced-motion mode removes animation.
Success is green with a check mark, errors are red with a warning icon, and
informational/progress messages use blue. Text explains the outcome independently
of color. Success and informational messages clear after five seconds, paused
while hovered with a mouse or focused. Errors and progress remain until dismissed,
replaced or a view change. Every message has a 44px dismiss control; keyboard
dismissal returns focus to the dialog's Close control. Older timers cannot clear
newer messages. The polite status region announces feedback without taking focus.
Closing the dialog clears its message and timers. Composer insertion receipts show the
result and hide control; font options return when editing a new draft.

The floating, dismissible message follows [Carbon's toast guidance](https://carbondesignsystem.com/components/notification/usage/).
Announcements follow the [W3C status message guidance](https://www.w3.org/WAI/WCAG22/Understanding/status-messages.html).

The dialog opens one list of text and images, ordered by their latest update.
Exact repeated text or image content reuses its existing item, moves it to the
top, and renews its expiry without shortening a longer retention choice.
Reading, copying an existing row, and pasting do not renew it. Dashboard
Clipboard settings select the default retention (initially 30 minutes). Each
row can set its expiry to 30 minutes, 4 hours, 1 day, 1 week, 30 days, or no
expiry. An explicit choice sets that duration from the current update time;
it can shorten a duration or replace No expiry. Repeating a choice restarts
that duration without adding the remaining time. Existing permanent snippets
remain permanent unless explicitly changed. Remaining time is orange below 15 minutes
and red below 5 minutes. Storage remains bounded; explicitly retained items
are protected from automatic eviction. See the server contract for limits.
Long countdowns show whole hours plus minutes, or days plus hours, instead of
rounding every partial hour/day upward. A small server/device clock difference
must not make a freshly selected four-hour duration read as five hours.

## Copying and importing

- Persea's terminal Copy writes to the device clipboard and independently adds
  a recent shared item. A shared-store outage must not prevent a successful
  local copy. Feedback distinguishes the two outcomes.
- A trusted browser Copy of selected text in Persea's terminal, frozen
  selection, composer, or Clipboard editor also shares that text. Ctrl+C
  without a selection retains its terminal interrupt behavior.
- Copying outside Persea is imported with **Paste from device**. The app
  does not monitor other applications or read the OS clipboard during polling.
  Browser permissions and trusted gestures govern clipboard access. When an
  API is unavailable, Add text accepts native paste and Add image opens the
  device picker. Native image paste inside the dialog also adds an image.
  An empty or refused read leaves the list open with a status message. Cancelling
  the device file picker does not cancel its parent Clipboard dialog.
- Copying an existing shared item does not create another copy or extend its
  deadline. Saving edited text updates its shared item, renews expiry, and
  merges an exact duplicate if one exists. Images can be copied when
  supported or downloaded. Safari's image write starts in the gesture with a
  promised PNG representation, before the authenticated image fetch settles.
- When a terminal app's automatic copy cannot reach the device clipboard,
  Clipboard shows a persistent retry notice. Its Copy action is bound to that
  exact value and only retries local delivery. Failed, unrelated, and stale
  copies do not clear the notice; a matching successful copy does.

## Terminal authority and cancellation

Opening text enters an editor with Save and Cancel. Save updates shared storage;
Cancel returns to the list. Neither sends terminal input. Rows expose separate
copy, paste, and delete actions; Delete is visually distinct. Text Paste uses
the existing input boundary and adds no Enter. The existing unsafe multiline
policy can route text to the composer for further review.

To attach an image, the client fetches its original bytes after the operator
asks to add it to the composer.
The dialog lifetime and destination identity must still match after that fetch.
Existing composer staging handles realm ownership, limits, cancellation, and
the final attachment path. Removing an attachment before Send sends no input.

There is no universal terminal undo after delivery: shells and terminal apps
can process bytes immediately. Do not implement guessed backspaces, Ctrl+U,
or another control sequence as a generic undo. Cancellation belongs before
the input boundary.

## Ownership and verification

`SnippetService` owns the document's text snapshot, mutations, revision
reconciliation, and OSC 52 coalescing. `ClipboardImages` owns a separate binary
API client. `ClipboardPreferencesService` owns the versioned default retention
and reconciles concurrent changes. Each content client has one single-flight poll loop while a visible view is
retained; hiding the document pauses polling and returning refreshes it.
The dialog owns its view, focus return, pending gestures, and operation lifetime.
Polling must not replace a row under a held pointer, disrupt an open native
expiry selector, or take focus from an editor.
Native selectors use focus as their ownership boundary: their OS popup may
consume pointer release outside the document, so they must not enter the
held-pointer set. A committed selection blurs the selector and shows saving
feedback until the service has reconciled the new revision.

`TerminalKeysPanel` keeps advanced configured sequences available in keyboard
settings and explicit favorites. Everyday All keys exposes ordinary groups;
it does not present the former Sequences/prefix chooser. Prefix bytes are sent
to the terminal program, where a nested tmux or screen instance may use them;
they do not control Persea's outer tmux. Panel background taps preserve the
active terminal/composer editor without preventing a native scrolling gesture.

Run `npm run test:clipboard-browser` for the Chromium/WebKit cross-device text,
image, cancellation, expiry, and access-boundary checks. Existing ergonomics,
workspace, session-switch, scroll-geometry, and key-panel gates retain their
original input and layout guarantees. OS clipboard prompts and the native
iPhone software keyboard require hardware verification; desktop WebKit alone
does not prove them.
