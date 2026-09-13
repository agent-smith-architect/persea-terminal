import { isStorableSnippetBody, snippetLabelFromBody, type SnippetOutcome, type SnippetRecord, type SnippetServicePort, type SnippetSnapshot } from "./snippet_client";
import { CLIPBOARD_IMAGE_TYPES, ClipboardImages, clipboardImageErrorMessage, clipboardImageFilename, clipboardImageURL, documentClipboardImages, type ClipboardImage, type ClipboardImagesSnapshot } from "./clipboard_images";
import { ClipboardPreferencesService, documentClipboardPreferences } from "./clipboard_preferences";
import { RETENTION_SECONDS, clipboardRetentionLabel, isClipboardRetentionSeconds, type ClipboardRetentionSeconds } from "./clipboard_retention";
import { clipboardIcon, type ClipboardIcon } from "./clipboard_icons";
import { ClipboardFeedback, type ClipboardNoticeKind } from "./clipboard_feedback";
import "./clipboard_panel.css";

type Delivery = Readonly<{ accepted: boolean; message: string; afterClose?: () => void }>;
type TerminalDestination = Readonly<{
  identity(): string;
  unavailable(): string;
  send(text: string, event: Event): Delivery;
  canAttach(): boolean;
  attach(file: File, event: Event): Delivery;
}>;
type Options = Readonly<{ text: SnippetServicePort; images?: ClipboardImages; preferences?: ClipboardPreferencesService; terminal?: TerminalDestination }>;
type Item = Readonly<{ kind: "text"; record: SnippetRecord }> | Readonly<{ kind: "image"; record: ClipboardImage }>;
type View = "list" | "text" | "image" | "add" | "settings";

function el<K extends keyof HTMLElementTagNameMap>(tag: K, className = "", text = ""): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag); node.className = className; node.textContent = text; return node;
}
function textOutcome(outcome: SnippetOutcome): string {
  if (outcome === "full") return "Your clipboard is full. Delete an item before adding another.";
  if (outcome === "too_large") return "Shared text can contain up to 16 KB.";
  if (outcome === "conflict") return "This item changed on another device. Go back to the list to see the latest version.";
  if (outcome === "refused") return "This change could not be saved. Reload the page and try again.";
  return "Shared text is unavailable. Check your connection and try again.";
}
function remaining(expiresAt: string | null): string {
  if (!expiresAt) return "No expiry";
  const seconds = Math.ceil((Date.parse(expiresAt) - Date.now()) / 1000);
  if (seconds <= 0) return "Expired";
  if (seconds < 60) return "Less than 1 min";
  if (seconds < 3600) return Math.ceil(seconds / 60) + " min left";
  if (seconds < 86400) {
    const hours = Math.floor(seconds / 3600), minutes = Math.floor(seconds % 3600 / 60);
    return hours + " h" + (minutes ? " " + minutes + " min" : "") + " left";
  }
  const days = Math.floor(seconds / 86400), hours = Math.floor(seconds % 86400 / 3600);
  return days + " d" + (hours ? " " + hours + " h" : "") + " left";
}
function urgency(expiresAt: string | null): string {
  if (!expiresAt) return "none";
  const left = Date.parse(expiresAt) - Date.now();
  return left < 300_000 ? "soon" : left < 900_000 ? "near" : "normal";
}
function live(item: Item): boolean { return !item.record.expiresAt || Date.parse(item.record.expiresAt) > Date.now(); }
function itemID(item: Item): string { return item.kind + ":" + item.record.id; }
function textBody(value: string): string { return value.replace(/\r\n?/g, "\n"); }
function itemTitle(item: Item): string {
  return item.kind === "image" ? "Image" : snippetLabelFromBody(item.record.body) || "Text";
}

let panelSerial = 0;

/** Shared clipboard management and explicit terminal insertion are separate
 * actions. Editing and saving an item never writes to a terminal. */
export class ClipboardPanel {
  readonly element = el("dialog", "persea-clipboard");
  private readonly images: ClipboardImages;
  private readonly preferences: ClipboardPreferencesService;
  private readonly body = el("div", "persea-clipboard__body");
  private readonly content = el("div", "persea-clipboard__content");
  private readonly handoff = el("section", "persea-clipboard__handoff");
  private handoffBody: string | undefined;
  private readonly footer = el("div", "persea-clipboard__footer");
  private readonly feedback = new ClipboardFeedback(() => {
    this.element.querySelector<HTMLButtonElement>("[aria-label='Close clipboard']")?.focus({ preventScroll: true });
  });
  private readonly title = el("h2", "", "Clipboard");
  private readonly back: HTMLButtonElement;
  private readonly settings: HTMLButtonElement;
  private readonly list = el("div", "persea-clipboard__list");
  private readonly fileInput = el("input");
  private textSnapshot: SnippetSnapshot;
  private imageSnapshot: ClipboardImagesSnapshot;
  private releases: (() => void)[] = [];
  private returnFocus?: HTMLElement;
  private view: View = "list";
  private query = "";
  private listKey = "";
  private busy = false;
  private lifetime = 0;
  private targetIdentity = "";
  private heldPointers = new Set<number>();
  private pendingList = false;
  private clock?: ReturnType<typeof setInterval>;
  private editorItem?: string;
  private readonly fitViewport = (): void => {
    if (!this.element.open) return;
    const viewport = window.visualViewport;
    const width = viewport?.width ?? window.innerWidth, height = viewport?.height ?? window.innerHeight;
    const mobile = width <= 480;
    const safeTop = Number.parseFloat(getComputedStyle(this.element).getPropertyValue("--clipboard-safe-top")) || 0;
    const panelWidth = Math.max(1, Math.min(620, width - (mobile ? 0 : 32)));
    const panelHeight = Math.max(1, Math.min(800, height - (mobile ? 12 + safeTop : 40)));
    this.element.style.width = panelWidth + "px"; this.element.style.height = panelHeight + "px";
    this.element.style.setProperty("--clipboard-panel-height", panelHeight + "px");
    this.element.style.left = ((viewport?.offsetLeft ?? 0) + (width - panelWidth) / 2) + "px";
    this.element.style.top = ((viewport?.offsetTop ?? 0) + (mobile ? height - panelHeight : (height - panelHeight) / 2)) + "px";
  };
  private readonly pointerReleased = (event: PointerEvent): void => {
    this.heldPointers.delete(event.pointerId);
    // Keep the clicked row intact through the click following pointerup.
    window.setTimeout(() => { if (this.pendingList && !this.heldPointers.size) this.renderList(); }, 0);
  };
  constructor(private readonly options: Options) {
    this.images = options.images ?? documentClipboardImages();
    this.preferences = options.preferences ?? documentClipboardPreferences();
    this.textSnapshot = options.text.snapshot(); this.imageSnapshot = this.images.snapshot();
    const heading = "persea-clipboard-title-" + (++panelSerial); this.title.id = heading;
    this.element.setAttribute("aria-labelledby", heading);
    this.element.dataset.clipboardContext = options.terminal ? "terminal" : "dashboard";
    const header = el("header", "persea-clipboard__header");
    this.back = this.iconButton("back", "Back to clipboard", () => this.showList(this.editorItem));
    this.back.dataset.clipboardDismiss = "true";
    this.settings = this.iconButton("settings", "Clipboard settings", () => this.showSettings());
    const close = this.iconButton("close", "Close clipboard", () => this.close());
    close.dataset.clipboardDismiss = "true";
    header.append(this.back, this.title, this.settings, close);
    this.handoff.hidden = true; this.handoff.setAttribute("aria-label", "Terminal copy awaiting this device");
    this.body.append(this.handoff, this.content);
    this.list.setAttribute("role", "list"); this.list.setAttribute("aria-label", "Clipboard items");
    this.fileInput.type = "file"; this.fileInput.accept = CLIPBOARD_IMAGE_TYPES.join(","); this.fileInput.multiple = true; this.fileInput.hidden = true;
    this.fileInput.addEventListener("change", () => {
      const files = Array.from(this.fileInput.files ?? []); this.fileInput.value = "";
      if (files.length && this.element.open && !this.busy) void this.addImages(files);
    });
    this.fileInput.addEventListener("cancel", () => {
      this.fileInput.value = "";
      if (this.element.open && !this.busy) this.notice("No image selected.", "info");
    });
    this.element.append(header, this.feedback.element, this.body, this.footer, this.fileInput);
    this.element.addEventListener("cancel", (event) => {
      // A file input's cancelled native picker bubbles its own cancel event.
      // Only the dialog's cancellation (for example Escape) closes this sheet.
      if (event.target !== this.element) return;
      event.preventDefault(); this.close();
    });
    this.element.addEventListener("pointerdown", (event) => {
      // Native pickers can consume pointerup outside the document. Their
      // focus already protects them from list replacement; tracking their
      // pointer as well can strand every later publication indefinitely.
      if (!(event.target instanceof HTMLSelectElement)) this.heldPointers.add(event.pointerId);
    }, true);
    this.list.addEventListener("focusout", () => {
      window.setTimeout(() => { if (this.pendingList) this.renderList(); }, 0);
    });
    this.element.addEventListener("copy", (event) => {
      if (!event.isTrusted || !(event.target instanceof HTMLTextAreaElement)) return;
      const input = event.target, value = textBody(input.value.slice(input.selectionStart, input.selectionEnd));
      // Copying an existing item is use, not a retention renewal.
      if (value && isStorableSnippetBody(value) && !this.alreadyShared(value)) void this.options.text.createClip(value);
    });
  }
  private button(label: string, action: (event: Event) => void, accessible = label): HTMLButtonElement {
    const node = el("button", "", label); node.type = "button"; node.setAttribute("aria-label", accessible);
    node.addEventListener("pointerdown", (event) => {
      if (event.isTrusted && event.button === 0 && document.activeElement instanceof HTMLTextAreaElement && this.element.contains(document.activeElement)) event.preventDefault();
    });
    node.addEventListener("click", (event) => { if (event.isTrusted && (!this.busy || node.dataset.clipboardDismiss)) action(event); });
    return node;
  }
  private iconButton(icon: ClipboardIcon, label: string, action: (event: Event) => void): HTMLButtonElement {
    const button = this.button("", action, label); button.className = "persea-clipboard__icon"; button.title = label;
    button.append(clipboardIcon(icon)); return button;
  }
  open(opener?: HTMLElement): void {
    if (this.element.open) return;
    this.returnFocus = opener ?? (document.activeElement instanceof HTMLElement ? document.activeElement : undefined);
    this.targetIdentity = this.options.terminal?.identity() ?? "";
    this.lifetime++; this.busy = false; this.query = ""; this.editorItem = undefined;
    this.showList(); document.body.append(this.element); this.element.showModal(); this.fitViewport();
    this.element.querySelector<HTMLButtonElement>("[aria-label='Close clipboard']")?.focus({ preventScroll: true });
    document.addEventListener("pointerup", this.pointerReleased, true);
    document.addEventListener("pointercancel", this.pointerReleased, true);
    window.visualViewport?.addEventListener("resize", this.fitViewport);
    window.visualViewport?.addEventListener("scroll", this.fitViewport);
    window.addEventListener("resize", this.fitViewport);
    this.releases.push(
      this.options.text.subscribe((snapshot) => { this.textSnapshot = snapshot; this.renderList(); }).dispose,
      this.images.subscribe((snapshot) => { this.imageSnapshot = snapshot; this.renderList(); }),
      this.preferences.subscribe((snapshot) => {
        this.settings.title = snapshot.status === "ready" ? "Clipboard settings · New items: " + clipboardRetentionLabel(snapshot.defaultRetentionSeconds) : "Clipboard settings";
      }),
      this.options.text.retainLive().dispose,
      this.images.retainLive(),
    );
    void this.preferences.load();
    this.clock = setInterval(() => { if (document.visibilityState !== "hidden") this.renderList(); }, 15_000);
  }
  close(): void {
    this.lifetime++; this.releases.splice(0).forEach((release) => release());
    document.removeEventListener("pointerup", this.pointerReleased, true);
    document.removeEventListener("pointercancel", this.pointerReleased, true);
    window.visualViewport?.removeEventListener("resize", this.fitViewport);
    window.visualViewport?.removeEventListener("scroll", this.fitViewport);
    window.removeEventListener("resize", this.fitViewport);
    if (this.clock !== undefined) clearInterval(this.clock);
    this.clock = undefined;
    this.heldPointers.clear(); this.pendingList = false; this.busy = false;
    if (this.element.open) this.element.close(); this.element.remove();
    this.content.replaceChildren(); this.handoff.replaceChildren(); this.handoff.hidden = true; this.handoffBody = undefined;
    this.footer.replaceChildren(); this.notice();
    if (this.returnFocus?.isConnected) this.returnFocus.focus({ preventScroll: true });
  }
  dispose(): void { this.close(); }
  private notice(text = "", kind: ClipboardNoticeKind = "error"): void {
    if (text) this.feedback.show(text, kind); else this.feedback.reset();
  }
  private setBusy(busy: boolean): void {
    this.busy = busy; this.element.setAttribute("aria-busy", String(busy));
    this.element.querySelectorAll<HTMLButtonElement | HTMLSelectElement>("button, select").forEach((control) => {
      control.disabled = (!control.dataset.clipboardDismiss && busy) || control.dataset.clipboardUnavailable === "true";
    });
  }
  private unavailable(): string {
    if (!this.options.terminal) return "";
    if (this.targetIdentity !== this.options.terminal.identity()) return "The terminal connection changed. Close and reopen Clipboard before pasting.";
    return this.options.terminal.unavailable();
  }
  private destinationButton(label: string, action: (event: Event) => void, reason = this.unavailable()): HTMLButtonElement {
    const button = this.button(label, action); button.className = "persea-clipboard__primary";
    if (reason) { button.disabled = true; button.dataset.clipboardUnavailable = "true"; button.title = reason; }
    return button;
  }
  private showList(focusItem?: string): void {
    this.view = "list"; this.lifetime++; this.editorItem = undefined;
    this.title.textContent = "Clipboard"; this.back.hidden = true; this.settings.hidden = !!this.options.terminal;
    this.notice(); this.listKey = ""; this.footer.replaceChildren();
    const search = el("input", "persea-clipboard__search");
    search.type = "search"; search.placeholder = "Search clipboard"; search.setAttribute("aria-label", "Find a clipboard item"); search.value = this.query;
    search.addEventListener("input", () => { this.query = search.value; this.renderList(); });
    this.content.replaceChildren(search, this.list);
    this.footer.classList.add("persea-clipboard__footer--tools");
    const tools: readonly [ClipboardIcon, string, () => void][] = [
      ["clipboard", "Paste from device", () => { void this.importClipboard(); }],
      ["text", "Add text", () => this.showAddText()],
      ["image", "Add image", () => { this.fileInput.value = ""; this.fileInput.click(); }],
    ];
    for (const [icon, label, action] of tools) {
      const button = this.button("", action, label); button.className = "persea-clipboard__tool"; button.title = label;
      button.append(clipboardIcon(icon), el("span", "", label)); this.footer.append(button);
    }
    this.setBusy(false); this.renderList();
    if (focusItem) this.focusItem(focusItem);
  }
  private allItems(): Item[] {
    const text = [...this.textSnapshot.snippets, ...this.textSnapshot.clips, ...(this.textSnapshot.osc ? [this.textSnapshot.osc] : [])];
    return [...text.map((record): Item => ({ kind: "text", record })), ...this.imageSnapshot.items.map((record): Item => ({ kind: "image", record }))]
      .filter(live).sort((a,b) => Date.parse(b.record.updatedAt) - Date.parse(a.record.updatedAt) || itemID(a).localeCompare(itemID(b)));
  }
  private items(): Item[] {
    const query = this.query.toLocaleLowerCase().trim();
    return this.allItems().filter((item) => !query || (item.kind === "text" ? item.record.label + "\n" + item.record.body + "\n" + item.record.origin : "image " + item.record.mediaType + " " + item.record.origin).toLocaleLowerCase().includes(query));
  }
  private focusItem(id: string): void {
    const row = Array.from(this.list.children).find((node) => node instanceof HTMLElement && node.dataset.clipboardItem === id);
    (row?.querySelector(".persea-clipboard__open") as HTMLButtonElement | null)?.focus({ preventScroll: true });
  }
  private renderList(): void {
    // Native select menus must survive background publications too.
    if (this.heldPointers.size || (document.activeElement instanceof HTMLSelectElement && this.list.contains(document.activeElement))) { this.pendingList = true; return; }
    this.pendingList = false; this.renderHandoff();
    if (this.view !== "list") return;
    const items = this.items();
    const key = JSON.stringify([this.query, Math.floor(Date.now()/15_000), this.textSnapshot.status, this.imageSnapshot.status, this.unavailable(), this.busy, items.map((item) => [itemID(item), item.record.revision, item.record.updatedAt, item.record.expiresAt, item.record.retentionSeconds])]);
    if (key === this.listKey) return;
    this.listKey = key;
    const focused = this.list.contains(document.activeElement) ? (document.activeElement as HTMLElement).dataset.clipboardFocus : undefined;
    const content: HTMLElement[] = [];
    if (this.textSnapshot.status === "unavailable") content.push(el("p", "persea-clipboard__note", "Shared text is unavailable. Try again when your connection returns."));
    if (this.imageSnapshot.status === "unavailable") content.push(el("p", "persea-clipboard__note", "Shared images are unavailable. You can still use text."));
    for (const item of items) {
      const id = itemID(item), row = el("div", "persea-clipboard__item");
      row.dataset.clipboardItem = id; row.setAttribute("role", "listitem");
      const open = this.button("", () => item.kind === "text" ? this.showText(item.record) : this.showImage(item.record), (item.kind === "text" ? "Edit text: " : "Preview ") + itemTitle(item));
      open.className = "persea-clipboard__open"; open.dataset.clipboardFocus = id + ":open";
      if (item.kind === "image") {
        const thumbnail = el("img", "persea-clipboard__thumbnail"); thumbnail.src = clipboardImageURL(item.record); thumbnail.alt = ""; thumbnail.loading = "lazy";
        open.append(thumbnail);
      }
      const text = el("span", "persea-clipboard__item-content");
      text.append(el("span", "persea-clipboard__preview", item.kind === "text" ? item.record.body : "Image"));
      const kind = item.kind === "image" ? item.record.mediaType.replace("image/", "").toUpperCase() + " · " + Math.ceil(item.record.byteSize/1024) + " KB" : "Text";
      text.append(el("span", "persea-clipboard__meta", [kind, item.record.origin].filter(Boolean).join(" · ")));
      const chevron = el("span", "persea-clipboard__chevron"); chevron.append(clipboardIcon("chevron")); open.append(text, chevron);
      const remove = this.iconButton("trash", "Delete item", () => { void this.remove(item); });
      remove.classList.add("persea-clipboard__delete"); remove.dataset.clipboardFocus = id + ":delete";
      const actions = el("div", "persea-clipboard__row-actions");
      const copy = this.iconButton("copy", "Copy to device", () => { void (item.kind === "text" ? this.copyText(item.record.body) : this.copyImage(item.record)); });
      copy.dataset.clipboardFocus = id + ":copy";
      if (item.kind === "image" && (!navigator.clipboard?.write || typeof ClipboardItem === "undefined")) {
        copy.dataset.clipboardUnavailable = "true"; copy.title = "Open the image to download it; this browser cannot copy images.";
      }
      actions.append(copy);
      if (this.options.terminal) {
        const reason = this.unavailable() || (item.kind === "image" && !this.options.terminal.canAttach() ? "Image attachments are unavailable for this terminal." : "");
        const paste = this.iconButton("paste", item.kind === "text" ? "Paste text to terminal" : "Insert image", (event) => {
          if (item.kind === "text") this.pasteText(item.record, event); else void this.attachImage(item.record, event);
        });
        paste.classList.add("persea-clipboard__paste"); paste.dataset.clipboardFocus = id + ":paste";
        if (reason) { paste.dataset.clipboardUnavailable = "true"; paste.title = reason; }
        actions.append(paste);
      }
      row.append(open, remove, this.expiryControl(item), actions); content.push(row);
    }
    if (!items.length) {
      const loading = this.textSnapshot.status === "idle" || this.textSnapshot.status === "loading" || this.imageSnapshot.status === "loading";
      content.push(el("p", "persea-clipboard__empty", loading ? "Loading your clipboard…" : this.query ? "No matching items." : "Your clipboard is empty. Paste from this device, add text, or choose an image below."));
    }
    this.list.replaceChildren(...content);
    this.setBusy(this.busy);
    if (focused) Array.from(this.list.querySelectorAll<HTMLElement>("[data-clipboard-focus]")).find((node) => node.dataset.clipboardFocus === focused)?.focus({ preventScroll: true });
  }
  private expiryControl(item: Item): HTMLElement {
    const wrapper = el("label", "persea-clipboard__expiry"); wrapper.dataset.urgency = urgency(item.record.expiresAt);
    const label = remaining(item.record.expiresAt); wrapper.append(el("span", "", label));
    const select = el("select"); select.setAttribute("aria-label", "Set expiry: " + label); select.title = "Set this item's expiry from now";
    select.dataset.clipboardFocus = itemID(item) + ":expiry";
    const current = el("option", "", label); current.value = ""; current.disabled = true; current.selected = true; select.append(current);
    for (const seconds of RETENTION_SECONDS) {
      const option = el("option", "", clipboardRetentionLabel(seconds) + (seconds ? " from now" : ""));
      option.value = String(seconds);
      select.append(option);
    }
    select.addEventListener("change", (event) => {
      if (!event.isTrusted || this.busy) return;
      const seconds = Number(select.value); select.value = ""; select.blur();
      if (isClipboardRetentionSeconds(seconds)) void this.setExpiry(item, seconds);
    });
    wrapper.append(select); return wrapper;
  }
  private renderHandoff(): void {
    const body = this.textSnapshot.clipboardHandoff?.body;
    if (body === this.handoffBody) return;
    this.handoffBody = body; this.handoff.hidden = body === undefined; this.handoff.replaceChildren();
    if (body === undefined) return;
    const message = el("p", "", "Terminal text is shared. Copy it to this device to use it in another app."); message.setAttribute("role", "status");
    const copy = this.iconButton("copy", "Copy terminal text to this device", () => { void this.copyText(body); }); copy.disabled = this.busy;
    this.handoff.append(message, copy);
  }
  private previewHeader(view: View, title: string): void {
    this.view = view; this.lifetime++; this.title.textContent = title; this.back.hidden = false; this.settings.hidden = true;
    this.notice(); this.footer.replaceChildren(); this.footer.classList.remove("persea-clipboard__footer--tools"); this.setBusy(false);
  }
  private showText(record: SnippetRecord): void {
    this.editorItem = "text:" + record.id; this.previewHeader("text", "Edit text");
    const input = el("textarea", "persea-clipboard__text"); input.value = record.body; input.spellcheck = false; input.setAttribute("aria-label", "Clipboard text");
    this.content.replaceChildren(input, el("p", "persea-clipboard__note", "Save updates this shared item and renews its expiry. It does not paste into the terminal."));
    const cancel = this.button("Cancel", () => this.showList("text:" + record.id)); cancel.dataset.clipboardDismiss = "true";
    this.footer.append(cancel, this.destinationButton("Save", () => { void this.saveText(record, input.value); }, ""));
  }
  private showImage(record: ClipboardImage): void {
    this.editorItem = "image:" + record.id; this.previewHeader("image", "Image");
    const image = el("img", "persea-clipboard__image"); image.src = clipboardImageURL(record); image.alt = "Shared clipboard image";
    const meta = el("p", "persea-clipboard__meta", Math.ceil(record.byteSize/1024) + " KB · " + remaining(record.expiresAt));
    const download = el("a", "persea-clipboard__download", "Download image"); download.href = clipboardImageURL(record); download.download = clipboardImageFilename(record);
    this.content.replaceChildren(image, meta);
    const done = this.button("Done", () => this.showList("image:" + record.id)); done.dataset.clipboardDismiss = "true";
    this.footer.append(download, done);
  }
  private showAddText(): void {
    this.editorItem = undefined; this.previewHeader("add", "Add text");
    const input = el("textarea", "persea-clipboard__text"); input.placeholder = "Type or paste text"; input.setAttribute("aria-label", "Text to add to shared clipboard");
    input.addEventListener("paste", (event) => {
      if (!event.isTrusted) return;
      const files = Array.from(event.clipboardData?.files ?? []).filter((file) => file.type.startsWith("image/"));
      if (!files.length) return;
      event.preventDefault();
      if (this.busy) { this.notice("Wait for the current image to finish, then paste the next one.", "progress"); return; }
      const text = event.clipboardData?.getData("text/plain") ?? "";
      if (text) input.setRangeText(text, input.selectionStart, input.selectionEnd, "end");
      void this.addImages(files, false);
    });
    this.content.replaceChildren(input, el("p", "persea-clipboard__note", "Available on your other devices. Matching content refreshes the existing item."));
    const cancel = this.button("Cancel", () => this.showList()); cancel.dataset.clipboardDismiss = "true";
    this.footer.append(cancel, this.destinationButton("Save", () => { void this.addText(input.value); }, ""));
    input.focus({ preventScroll: true });
  }
  private showSettings(): void {
    this.editorItem = undefined; this.previewHeader("settings", "Clipboard settings");
    const label = el("label", "persea-clipboard__setting", "Keep new items for");
    const select = el("select", "persea-clipboard__duration"); select.setAttribute("aria-label", "Default clipboard retention");
    select.id = this.title.id + "-retention"; label.htmlFor = select.id;
    for (const seconds of RETENTION_SECONDS) { const option = el("option", "", clipboardRetentionLabel(seconds)); option.value = String(seconds); select.append(option); }
    const snapshot = this.preferences.snapshot(); select.value = String(snapshot.defaultRetentionSeconds);
    this.content.replaceChildren(label, select, el("p", "persea-clipboard__note", "Applies across your devices when you add an item. Adding matching content keeps its longer expiry. Use an item's time badge to set its expiry from now, including a shorter duration."));
    const cancel = this.button("Cancel", () => this.showList()); cancel.dataset.clipboardDismiss = "true";
    const save = this.destinationButton("Save", () => {
      const seconds = Number(select.value); if (isClipboardRetentionSeconds(seconds)) void this.saveDefault(seconds);
    }, "");
    this.footer.append(cancel, save);
    if (snapshot.status !== "ready") {
      this.setBusy(true); this.notice("Loading clipboard settings…", "progress");
      const lifetime = this.lifetime;
      void this.preferences.load().then(() => {
        if (lifetime !== this.lifetime) return;
        const current = this.preferences.snapshot();
        this.setBusy(false);
        if (current.status === "ready") { select.value = String(current.defaultRetentionSeconds); this.notice(); }
        else { save.disabled = true; save.dataset.clipboardUnavailable = "true"; this.notice("Shared settings are unavailable. Close and reopen settings to try again."); }
      });
    }
  }
  private async saveDefault(seconds: ClipboardRetentionSeconds): Promise<void> {
    const lifetime = this.lifetime; this.setBusy(true);
    try {
      const outcome = await this.preferences.setDefault(seconds);
      if (lifetime !== this.lifetime) return;
      if (outcome === "ok") { this.showList(); this.notice("Default expiry updated for all your devices.", "success"); }
      else this.notice(outcome === "conflict" ? "Settings changed on another device. Cancel and reopen settings to see the latest value." : "Settings could not be saved. Try again.");
    } catch { if (lifetime === this.lifetime) this.notice("Settings could not be saved. Try again."); }
    finally { if (lifetime === this.lifetime) this.setBusy(false); }
  }
  private async saveText(record: SnippetRecord, value: string): Promise<void> {
    const body = textBody(value);
    if (!isStorableSnippetBody(body)) { this.notice("Enter nonempty text up to 16 KB, without terminal control characters."); return; }
    const lifetime = this.lifetime; this.setBusy(true);
    try {
      const outcome = await this.options.text.updateText(record, body);
      if (lifetime !== this.lifetime) return;
      if (outcome === "ok") { this.showList(); this.notice("Saved. Expiry renewed.", "success"); }
      else this.notice(textOutcome(outcome));
    } catch { if (lifetime === this.lifetime) this.notice(textOutcome("unavailable")); }
    finally { if (lifetime === this.lifetime) this.setBusy(false); }
  }
  private async addText(value: string): Promise<void> {
    const body = textBody(value);
    if (!isStorableSnippetBody(body)) { this.notice("Enter nonempty text up to 16 KB, without terminal control characters."); return; }
    const lifetime = this.lifetime; this.setBusy(true);
    try {
      const outcome = await this.options.text.createClip(body);
      if (lifetime !== this.lifetime) return;
      if (outcome === "ok") { this.showList(); this.notice("Clipboard updated.", "success"); }
      else this.notice(textOutcome(outcome));
    } catch { if (lifetime === this.lifetime) this.notice(textOutcome("unavailable")); }
    finally { if (lifetime === this.lifetime) this.setBusy(false); }
  }
  private pasteText(record: SnippetRecord, event: Event): void {
    const reason = this.unavailable(); if (reason) { this.notice(reason); return; }
    if (!live({ kind: "text", record })) { this.renderList(); this.notice("This item has expired. Choose another item."); return; }
    const result = this.options.terminal!.send(record.body, event);
    if (result.accepted) { this.close(); result.afterClose?.(); } else this.notice(result.message);
  }
  private async setExpiry(item: Item, seconds: ClipboardRetentionSeconds): Promise<void> {
    const lifetime = this.lifetime; this.setBusy(true); this.notice("Saving expiry…", "progress");
    try {
      if (item.kind === "image") await this.images.setRetention(item.record, seconds);
      else {
        const outcome = await this.options.text.setRetention(item.record, seconds);
        if (outcome !== "ok") { if (lifetime === this.lifetime) this.notice(textOutcome(outcome)); return; }
      }
      if (lifetime === this.lifetime) { this.showList(itemID(item)); this.notice(seconds === 0 ? "This item has no expiry." : "Expiry set to " + clipboardRetentionLabel(seconds) + " from now. Item moved to the top.", "success"); }
    } catch (error) { if (lifetime === this.lifetime) this.notice(item.kind === "image" ? clipboardImageErrorMessage(error) : textOutcome("unavailable")); }
    finally { if (lifetime === this.lifetime) this.setBusy(false); }
  }
  private async addImages(files: readonly Blob[], returnToList = true): Promise<void> {
    if (this.busy || !this.element.open) return;
    const lifetime = this.lifetime; this.setBusy(true); this.notice("Adding image…", "progress");
    let added = 0;
    try {
      for (const file of files) { await this.images.add(file); added++; if (lifetime !== this.lifetime) return; }
      if (returnToList) this.showList();
      this.notice(added === 1 ? "Image added. Matching content refreshes the existing item." : "Clipboard updated with " + added + " images.", "success");
    } catch (error) { if (lifetime === this.lifetime) this.notice((added ? added + " added. " : "") + clipboardImageErrorMessage(error)); }
    finally { if (lifetime === this.lifetime) this.setBusy(false); }
  }
  private async importClipboard(): Promise<void> {
    const lifetime = this.lifetime; let importing = false, added = 0;
    try {
      // Invoke the native API inside the click. Permission dismissal is an
      // ordinary cancellation, never an instruction to open another view.
      const read = navigator.clipboard?.read ? navigator.clipboard.read() : navigator.clipboard?.readText();
      if (!read) { this.notice("This browser cannot read the clipboard here. Add text and use your device's Paste command."); return; }
      this.setBusy(true); this.notice("Checking this device's clipboard…", "progress");
      const result = await read;
      if (lifetime !== this.lifetime) return;
      const images: Blob[] = [], texts: string[] = [];
      if (typeof result === "string") { if (result) texts.push(result); }
      else {
        for (const item of result) {
          const media = item.types.find((type) => CLIPBOARD_IMAGE_TYPES.some((allowed) => allowed === type));
          if (media) images.push(await item.getType(media));
          if (item.types.includes("text/plain")) { const value = await (await item.getType("text/plain")).text(); if (value) texts.push(value); }
        }
      }
      if (lifetime !== this.lifetime) return;
      if (!images.length && !texts.length) { this.notice("No text or image found on this device's clipboard.", "info"); return; }
      importing = true;
      for (const image of images) { await this.images.add(image); added++; if (lifetime !== this.lifetime) return; }
      for (const value of texts) {
        const body = textBody(value);
        if (!isStorableSnippetBody(body)) { this.notice((added ? added + " item(s) added. " : "") + "Text must be at most 16 KB, without terminal control characters."); return; }
        const outcome = await this.options.text.createClip(body);
        if (lifetime !== this.lifetime) return;
        if (outcome !== "ok") { this.notice((added ? added + " item(s) added. " : "") + textOutcome(outcome)); return; }
        added++;
      }
      this.showList(); this.notice(added === 1 ? "Clipboard updated from this device." : "Clipboard updated with " + added + " items.", "success");
    } catch (error) {
      if (lifetime !== this.lifetime) return;
      this.notice(importing ? (added ? added + " item(s) added. " : "") + clipboardImageErrorMessage(error) : "Nothing pasted. Clipboard access was cancelled or blocked. You can try again or use Add text.", importing ? "error" : "info");
    } finally { if (lifetime === this.lifetime) this.setBusy(false); }
  }
  private async copyText(value: string): Promise<void> {
    const lifetime = this.lifetime;
    try {
      const write = navigator.clipboard.writeText(value);
      this.setBusy(true); await write; this.options.text.acknowledgeClipboard(value);
      if (lifetime === this.lifetime) this.notice("Copied to this device.", "success");
    } catch { if (lifetime === this.lifetime) this.notice("Copy was refused. Open the text, select it, and use your device's Copy command."); }
    finally { if (lifetime === this.lifetime) this.setBusy(false); }
  }
  private alreadyShared(body: string): boolean {
    return this.allItems().some((item) => item.kind === "text" && item.record.body === body);
  }
  private async png(image: ClipboardImage): Promise<Blob> {
    const file = await this.images.file(image);
    if (file.type === "image/png") return file;
    const bitmap = await createImageBitmap(file);
    try {
      const canvas = el("canvas"); canvas.width = bitmap.width; canvas.height = bitmap.height;
      const context = canvas.getContext("2d"); if (!context) throw new Error("Image conversion unavailable");
      context.drawImage(bitmap, 0, 0);
      return await new Promise<Blob>((resolve, reject) => canvas.toBlob((blob) => blob ? resolve(blob) : reject(new Error("Image conversion unavailable")), "image/png"));
    } finally { bitmap.close(); }
  }
  private async copyImage(image: ClipboardImage): Promise<void> {
    const lifetime = this.lifetime;
    try {
      // Safari needs write() in the original gesture. A promised representation
      // lets the authenticated image fetch finish afterwards.
      const representation = this.png(image); void representation.catch(() => undefined);
      const write = navigator.clipboard.write([new ClipboardItem({ "image/png": representation })]);
      this.setBusy(true); await write;
      if (lifetime === this.lifetime) this.notice("Image copied to this device.", "success");
    } catch { if (lifetime === this.lifetime) this.notice("This browser could not copy the image. Open it and use Download image."); }
    finally { if (lifetime === this.lifetime) this.setBusy(false); }
  }
  private async attachImage(image: ClipboardImage, event: Event): Promise<void> {
    const reason = this.unavailable(); if (reason) { this.notice(reason); return; }
    const lifetime = this.lifetime; this.setBusy(true); this.notice("Preparing the image…", "progress");
    try {
      const file = await this.images.file(image);
      if (lifetime !== this.lifetime || !this.element.open) return;
      const currentReason = this.unavailable(); if (currentReason) { this.notice(currentReason); return; }
      const result = this.options.terminal!.attach(file, event);
      if (result.accepted) { this.close(); result.afterClose?.(); } else this.notice(result.message);
    } catch (error) { if (lifetime === this.lifetime) this.notice(clipboardImageErrorMessage(error)); }
    finally { if (lifetime === this.lifetime) this.setBusy(false); }
  }
  private async remove(item: Item): Promise<void> {
    const lifetime = this.lifetime; this.setBusy(true);
    try {
      if (item.kind === "image") await this.images.remove(item.record);
      else { const outcome = await this.options.text.remove(item.record); if (outcome !== "ok") { if (lifetime === this.lifetime) this.notice(textOutcome(outcome)); return; } }
      if (lifetime === this.lifetime) { this.showList(); this.notice("Item deleted.", "success"); }
    } catch (error) { if (lifetime === this.lifetime) this.notice(clipboardImageErrorMessage(error)); }
    finally { if (lifetime === this.lifetime) this.setBusy(false); }
  }
}
