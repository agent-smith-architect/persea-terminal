import { actionGlyph, KEY_GROUPS, KEY_PRESETS, keyAction, keyGlyph, NO_MODIFIERS, planKey, prefixSequence, resolveAction, validPrefixName, type KeyModifiers } from "./terminal_actions";
import { canonicalKeyboardAction, DEFAULT_KEYBOARD_PREFERENCES, documentKeyboardPreferences, type KeyboardPreferences, type KeyboardPreferencesService, type KeyboardPreferenceSnapshot } from "./keyboard_preferences";
import "./keyboard_settings.css";

type Scope = "shared" | "device";
type Section = "bar" | "favorites" | "prefixes";
type Draft = { preferences: KeyboardPreferences; layoutOverride: boolean; prefixesOverride: boolean; dirty: boolean; revision: number; baseline: string; conflict: boolean };
const clone = (value: KeyboardPreferences): KeyboardPreferences => ({ version: 1, layout: { bar: [...value.layout.bar], favorites: [...value.layout.favorites] }, prefixes: { ...value.prefixes } });
function el<K extends keyof HTMLElementTagNameMap>(tag: K, className = "", text = ""): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag); node.className = className; node.textContent = text; return node;
}
function button(text: string, action: () => void, label = text): HTMLButtonElement {
  const node = el("button", "", text); node.type = "button"; node.setAttribute("aria-label", label); node.title = label; node.dataset.settingsControl = label; node.addEventListener("click", action); return node;
}
export function keyboardIcon(kind: "settings" | "hide"): SVGSVGElement {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24"); svg.setAttribute("width", "20"); svg.setAttribute("height", "20"); svg.setAttribute("fill", "none"); svg.setAttribute("stroke", "currentColor"); svg.setAttribute("stroke-width", "1.6"); svg.setAttribute("stroke-linecap", "round"); svg.setAttribute("stroke-linejoin", "round"); svg.setAttribute("aria-hidden", "true");
  const path = document.createElementNS(svg.namespaceURI, "path");
  path.setAttribute("d", kind === "hide" ? "M4 3h16v12H4z M7 6h.1m3 0h.1m3 0h.1m3 0h.1M7 9h.1m3 0h.1m3 0h.1m3 0h.1M8 12h8 M8 19l4 3 4-3" : "M9 3h6l.5 3 2 .9 2.5-1.2 3 5.2-2.4 1.8v2.4l2.4 1.8-3 5.2-2.5-1.2-2 .9-.5 3H9l-.5-3-2-.9L4 21.1l-3-5.2 2.4-1.8v-2.4L1 9.9l3-5.2 2.5 1.2 2-.9L9 3 M16 13a4 4 0 1 1-8 0 4 4 0 0 1 8 0");
  // The gear is centered in a slightly larger viewbox to leave breathing room.
  if (kind === "settings") svg.setAttribute("viewBox", "-1 1 26 26");
  svg.append(path); return svg;
}

/** One full-size editor for the dashboard and every terminal. It edits a draft;
 * its catalog never has a terminal input callback. Native dialog provides the
 * focus boundary and inert background without touching a terminal draft. */
export class KeyboardSettings {
  readonly element = el("dialog", "keyboard-settings");
  private scope: Scope = "shared";
  private section: Section = "bar";
  private drafts!: Record<Scope, Draft>;
  private catalog = false;
  private group = "Navigation";
  private modifiers: KeyModifiers = NO_MODIFIERS;
  private prefix = "tmux";
  private prefixName = "";
  private editingPrefix = "";
  private status = "";
  private busy = false;
  private confirmingClose = false;
  private unsubscribe?: () => void;
  private returnFocus?: HTMLElement;
  private transportState = "";
  private pendingRender = false;
  private pendingRenderTimer?: number;
  private nextFocus?: string;
  private readonly heldPointers = new Set<number>();
  private readonly releasePointer = (event: PointerEvent): void => { this.heldPointers.delete(event.pointerId); this.schedulePendingRender(); };
  private headingID = `keyboard-settings-${++settingsSerial}`;
  constructor(private readonly service: KeyboardPreferencesService = documentKeyboardPreferences()) {
    this.element.setAttribute("aria-labelledby", this.headingID);
    this.element.tabIndex = -1;
    this.element.addEventListener("cancel", (event) => { event.preventDefault(); this.requestClose(); });
    this.element.addEventListener("pointerdown", (event) => this.heldPointers.add(event.pointerId), true);
    this.element.addEventListener("focusout", () => this.schedulePendingRender());
  }
  private schedulePendingRender(): void {
    if (!this.pendingRender || this.pendingRenderTimer !== undefined) return;
    this.pendingRenderTimer = window.setTimeout(() => {
      this.pendingRenderTimer = undefined;
      // A focusout often belongs to the beginning of a button press. Keep
      // that button alive through release/click, then apply the publication.
      if (this.element.open && this.pendingRender && !this.editableFocused() && this.heldPointers.size === 0) this.render();
    }, 0);
  }
  open(returnFocus?: HTMLElement): void {
    if (this.element.open) return;
    this.returnFocus = returnFocus ?? (document.activeElement instanceof HTMLElement ? document.activeElement : undefined);
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
    const snapshot = this.service.snapshot();
    this.transportState = JSON.stringify([snapshot.available, snapshot.loaded, snapshot.message]);
    this.drafts = { shared: this.makeDraft("shared", snapshot), device: this.makeDraft("device", snapshot) };
    this.scope = "shared"; this.section = "bar"; this.catalog = false; this.editingPrefix = ""; this.status = ""; this.confirmingClose = false;
    this.render(); document.body.append(this.element); this.element.showModal();
    document.addEventListener("pointerup", this.releasePointer, true);
    document.addEventListener("pointercancel", this.releasePointer, true);
    this.element.querySelector<HTMLButtonElement>("[data-settings-close]")?.focus({ preventScroll: true });
    this.unsubscribe = this.service.subscribe((next) => this.receive(next));
    void this.service.load();
  }
  private makeDraft(scope: Scope, snapshot: KeyboardPreferenceSnapshot): Draft {
    return { preferences: clone(scope === "shared" ? snapshot.shared : snapshot.effective), layoutOverride: !!snapshot.device.layout, prefixesOverride: !!snapshot.device.prefixes, dirty: false, revision: snapshot.revision, baseline: this.signature(scope, snapshot), conflict: false };
  }
  private signature(scope: Scope, snapshot: KeyboardPreferenceSnapshot): string { return JSON.stringify(scope === "shared" ? snapshot.shared : { shared: snapshot.shared, device: snapshot.device }); }
  private editableFocused(): boolean {
    const active = document.activeElement;
    return this.element.contains(active) && (active instanceof HTMLInputElement || active instanceof HTMLTextAreaElement || active instanceof HTMLSelectElement || (active instanceof HTMLElement && active.isContentEditable));
  }
  private receive(snapshot: KeyboardPreferenceSnapshot): void {
    if (!this.element.open || this.busy) return;
    const transport = JSON.stringify([snapshot.available, snapshot.loaded, snapshot.message]);
    let changed = this.transportState !== transport;
    this.transportState = transport;
    for (const scope of ["shared", "device"] as const) {
      const draft = this.drafts[scope];
      if (draft.baseline === this.signature(scope, snapshot)) { draft.revision = snapshot.revision; continue; }
      if (draft.dirty || (scope === this.scope && this.editableFocused())) draft.conflict = true;
      else this.drafts[scope] = this.makeDraft(scope, snapshot);
      changed = true;
    }
    if (changed) {
      // External publications must not replace a field under a native IME or
      // dictation transaction. Keep the exact node until editing focus leaves.
      if (this.editableFocused() || this.heldPointers.size > 0) {
        this.pendingRender = true;
        const status = this.element.querySelector<HTMLElement>(".keyboard-settings__status");
        if (status && this.draft.conflict) status.textContent = "Saved settings changed. Your current edit is preserved; review the update when you finish typing.";
      } else this.render();
    }
  }
  private get draft(): Draft { return this.drafts[this.scope]; }
  private update(preferences: KeyboardPreferences): void {
    this.draft.preferences = preferences; this.draft.dirty = true; this.status = "";
    if (this.scope === "device") { if (this.section === "prefixes") this.draft.prefixesOverride = true; else this.draft.layoutOverride = true; }
    this.render();
  }
  private requestClose(): void {
    if (this.busy) return;
    if (this.drafts.shared.dirty || this.drafts.device.dirty || this.prefixName.trim()) { this.confirmingClose = true; this.nextFocus = "Keep editing"; this.render(); const body = this.element.querySelector<HTMLElement>(".keyboard-settings__body"); if (body) body.scrollTop = 0; return; }
    this.close();
  }
  private close(): void {
    this.pendingRender = false; if (this.pendingRenderTimer !== undefined) clearTimeout(this.pendingRenderTimer); this.pendingRenderTimer = undefined;
    this.heldPointers.clear(); document.removeEventListener("pointerup", this.releasePointer, true); document.removeEventListener("pointercancel", this.releasePointer, true);
    this.unsubscribe?.(); this.unsubscribe = undefined; this.element.close(); this.element.remove();
    // Returning to an ordinary control avoids reopening the software keyboard.
    if (this.returnFocus?.isConnected && !(this.returnFocus instanceof HTMLTextAreaElement || this.returnFocus instanceof HTMLInputElement)) this.returnFocus.focus({ preventScroll: true });
  }
  dispose(): void { if (this.element.open) this.close(); else this.element.remove(); }
  private async save(): Promise<void> {
    if (this.busy || this.draft.conflict) return;
    const scope = this.scope, draft = this.draft;
    if (scope === "device") {
      this.busy = true;
      this.status = this.service.saveDevice({ version: 1, ...(draft.layoutOverride ? { layout: draft.preferences.layout } : {}), ...(draft.prefixesOverride ? { prefixes: draft.preferences.prefixes } : {}) });
      this.busy = false; this.drafts.device = this.makeDraft("device", this.service.snapshot()); this.render(); return;
    }
    this.busy = true; this.render();
    const result = await this.service.saveShared(draft.preferences, draft.revision);
    this.busy = false;
    if (result === "saved") { this.drafts.shared = this.makeDraft("shared", this.service.snapshot()); this.status = "Saved as the default for all devices. Device customizations are retained."; }
    else if (result === "conflict") { draft.conflict = true; this.status = "Shared settings changed while you were editing. Review the latest layout below."; }
    else this.status = "Shared settings could not be saved. Your edits are still here; try again.";
    this.receive(this.service.snapshot()); this.render();
  }
  private render(): void {
    this.pendingRender = false;
    const scroll = this.element.querySelector<HTMLElement>(".keyboard-settings__body")?.scrollTop ?? 0;
    const activeElement = this.element.contains(document.activeElement) ? document.activeElement as HTMLElement : undefined;
    const active = this.nextFocus ?? activeElement?.dataset.settingsControl;
    this.nextFocus = undefined;
    const selection = activeElement instanceof HTMLInputElement && activeElement.type === "text" ? { start: activeElement.selectionStart, end: activeElement.selectionEnd, direction: activeElement.selectionDirection } : undefined;
    const header = el("header", "keyboard-settings__header");
    const heading = el("h2", "", "Keyboard"); heading.id = this.headingID;
    const close = button("×", () => this.requestClose(), "Close keyboard settings"); close.dataset.settingsClose = ""; close.dataset.settingsControl = "close"; close.disabled = this.busy;
    header.append(heading, close);
    const scope = el("div", "keyboard-settings__scope"); scope.setAttribute("role", "group"); scope.setAttribute("aria-label", "Settings scope");
    for (const [id, label] of [["shared", "All devices"], ["device", "This device"]] as const) {
      const control = button(label, () => { this.scope = id; this.catalog = false; this.editingPrefix = ""; this.status = ""; this.render(); });
      control.setAttribute("aria-pressed", String(this.scope === id)); control.dataset.settingsControl = `scope:${id}`; control.disabled = this.busy; scope.append(control);
    }
    const note = el("p", "keyboard-settings__scope-note", this.scope === "shared" ? "Defaults for every terminal and device." : "All terminals in this browser. Other devices keep their own settings.");
    const body = el("div", "keyboard-settings__body");
    if (this.confirmingClose) {
      const confirmation = el("div", "keyboard-settings__notice", "Discard unsaved changes?");
      confirmation.append(button("Keep editing", () => { this.confirmingClose = false; this.nextFocus = "close"; this.render(); }), button("Discard changes", () => this.close())); body.append(confirmation);
    }
    if (this.draft.conflict) this.renderConflict(body);
    if (this.scope === "device") {
      const inheritance = el("div", "keyboard-settings__inheritance");
      for (const [field, label] of [["layoutOverride", "Customize layout on this device"], ["prefixesOverride", "Customize prefixes on this device"]] as const) {
        const wrap = el("label"), input = el("input"); input.type = "checkbox"; input.checked = this.draft[field];
        input.dataset.settingsControl = field;
        input.addEventListener("change", () => { this.draft[field] = input.checked; this.draft.dirty = true; if (!input.checked) this.draft.preferences = { ...this.draft.preferences, ...(field === "layoutOverride" ? { layout: this.service.snapshot().shared.layout } : { prefixes: this.service.snapshot().shared.prefixes }) }; this.render(); });
        wrap.append(input, document.createTextNode(label)); inheritance.append(wrap);
      }
      inheritance.append(button("Use shared defaults", () => { this.draft.layoutOverride = this.draft.prefixesOverride = false; this.draft.preferences = clone(this.service.snapshot().shared); this.draft.dirty = true; this.status = "Save to remove this device's customizations."; this.render(); })); body.append(inheritance);
    }
    const preview = el("section", "keyboard-settings__preview"); preview.setAttribute("aria-label", "Key bar preview");
    preview.append(el("span", "keyboard-settings__eyebrow", "ABOVE YOUR PHONE KEYBOARD"));
    const previewRow = el("div", "keyboard-settings__preview-row");
    const previewKeys = el("div", "keyboard-settings__preview-keys");
    for (const id of this.draft.preferences.layout.bar) {
      const key = el("span", id.startsWith("modifier:") ? "keyboard-settings__key keyboard-settings__key--modifier" : "keyboard-settings__key", this.label(id));
      key.dataset.keyGlyph = String(key.textContent?.length === 1); previewKeys.append(key);
    }
    previewRow.append(previewKeys, el("span", "keyboard-settings__key keyboard-settings__key--navigation", "Keys")); preview.append(previewRow); body.append(preview);
    const tabs = el("div", "keyboard-settings__tabs"); tabs.setAttribute("role", "group"); tabs.setAttribute("aria-label", "Keyboard settings section");
    for (const [id, label] of [["bar", "Key bar"], ["favorites", "Favorites"], ["prefixes", "Prefixes"]] as const) {
      const control = button(label, () => { this.section = id; this.catalog = false; this.editingPrefix = ""; this.render(); }); control.setAttribute("aria-pressed", String(this.section === id)); control.dataset.settingsControl = `section:${id}`; tabs.append(control);
    }
    body.append(tabs);
    if (this.section === "prefixes") this.renderPrefixes(body); else this.renderList(body);
    const help = el("details", "keyboard-settings__help");
    help.append(el("summary", "", "How terminal keys work"), el("p", "", "Ctrl, Alt and Shift apply to the next key you tap in Keys, or the next A–Z letter you type directly into the terminal. Tap a selected modifier again to cancel it. Composer text, paste and composition keep their normal behavior."), el("p", "", "Alt sends a terminal Meta key. The app in the terminal decides what that key does. Classic terminal input shares some codes: Ctrl+Shift+C, for example, sends the same code as Ctrl+C. Dimmed combinations cannot be sent by this terminal's keyboard engine; their labels explain why."), el("p", "", "Prefixes are explicit key sequences, such as Ctrl+B followed by N. They use your configuration; Persea does not detect or change an application's bindings.")); body.append(help);
    const footer = el("footer", "keyboard-settings__footer");
    const status = el("p", "keyboard-settings__status", this.status || (this.scope === "shared" && !this.service.snapshot().available ? this.service.snapshot().message || "Loading shared settings…" : "")); status.setAttribute("role", "status");
    const restore = button("Restore factory settings", () => { if (this.scope === "device") this.draft.layoutOverride = this.draft.prefixesOverride = true; this.update(clone(DEFAULT_KEYBOARD_PREFERENCES)); }, "Restore factory keyboard settings in this draft");
    restore.className = "keyboard-settings__restore";
    body.append(restore);
    const save = button(this.busy ? "Saving…" : this.scope === "shared" ? "Save for all devices" : "Save for this device", () => void this.save()); save.className = "keyboard-settings__save"; save.disabled = this.busy || !this.draft.dirty || this.draft.conflict || (this.scope === "shared" && !this.service.snapshot().available);
    footer.append(status, save);
    if (this.busy) { body.inert = true; restore.disabled = true; }
    this.element.replaceChildren(header, scope, note, body, footer); body.scrollTop = scroll;
    if (active) {
      const controls = Array.from(this.element.querySelectorAll<HTMLElement>("[data-settings-control]"));
      const target = controls.find((node) => node.dataset.settingsControl === active && !(node instanceof HTMLButtonElement && node.disabled)) ?? controls.find((node) => node.dataset.settingsControl === `section:${this.section}`) ?? close;
      target.focus({ preventScroll: true });
      if (selection && target instanceof HTMLInputElement && target.type === "text" && selection.start !== null && selection.end !== null) target.setSelectionRange(selection.start, selection.end, selection.direction ?? undefined);
    }
    if (this.busy && this.element.open) this.element.focus({ preventScroll: true });
  }
  private label(id: string): string { return id.startsWith("modifier:") ? ({ ctrl: "Ctrl", alt: "Alt", shift: "⇧" } as Record<string, string>)[id.slice(9)] : (resolveAction(id) ? actionGlyph(resolveAction(id)!) : "Unavailable"); }
  private renderConflict(body: HTMLElement): void {
    const latest = this.service.snapshot();
    const notice = el("section", "keyboard-settings__notice"); notice.setAttribute("aria-label", "Settings changed elsewhere");
    notice.append(el("p", "", "Settings changed elsewhere. Your unsaved edits are preserved."));
    const details = el("details"); details.append(el("summary", "", "Review current saved settings"));
    const preferences = this.scope === "shared" ? latest.shared : latest.effective;
    details.append(el("p", "", `Key bar: ${preferences.layout.bar.map((id) => this.label(id)).join(" · ") || "Empty"}`), el("p", "", `Favorites: ${preferences.layout.favorites.map((id) => this.label(id)).join(" · ") || "Empty"}`), el("p", "", `Prefixes: ${Object.entries(preferences.prefixes).map(([name, id]) => `${name}: ${this.label(id)}`).join(" · ") || "None"}`));
    notice.append(details, button("Use latest", () => { this.drafts[this.scope] = this.makeDraft(this.scope, latest); this.status = "Latest settings loaded."; this.render(); }), button("Keep my edits", () => { this.draft.revision = latest.revision; this.draft.baseline = this.signature(this.scope, latest); this.draft.conflict = false; this.status = "Reviewed. Save to apply your edits over the latest settings."; this.render(); })); body.append(notice);
  }
  private renderList(body: HTMLElement): void {
    const section = this.section as "bar" | "favorites";
    body.append(el("p", "keyboard-settings__note", section === "bar" ? "Order the keys beside Keys. Swipe the row for more." : "Quick choices when you open Keys: keys, chords and prefix sequences."));
    const tools = el("div", "keyboard-settings__list-tools");
    tools.append(button(this.catalog ? "Close key picker" : "Add key", () => { this.catalog = !this.catalog; this.editingPrefix = ""; this.render(); }, this.catalog ? "Close key picker" : "Add key or chord"));
    const preset = el("select"); preset.setAttribute("aria-label", "Keyboard preset"); preset.dataset.settingsControl = "preset"; const first = el("option", "", "Preset…"); first.value = ""; preset.append(first);
    for (const name of Object.keys(KEY_PRESETS)) { const option = el("option", "", name); option.value = name; preset.append(option); }
    preset.addEventListener("change", () => {
      const source = KEY_PRESETS[preset.value];
      if (!source) return;
      const keys = section === "favorites" && preset.value === "Terminal" ? DEFAULT_KEYBOARD_PREFERENCES.layout.favorites : source.map(canonicalKeyboardAction).filter((id): id is string => !!id);
      this.update({ ...this.draft.preferences, layout: { ...this.draft.preferences.layout, [section]: keys } });
    }); tools.append(preset); body.append(tools);
    if (this.catalog) this.renderCatalog(body);
    const list = el("ol", "keyboard-settings__list");
    const items = this.draft.preferences.layout[section];
    items.forEach((id, index) => {
      const row = el("li", "keyboard-settings__row"), label = this.label(id);
      row.append(el("span", "keyboard-settings__key-name", label));
      for (const [glyph, delta] of [["↑", -1], ["↓", 1]] as const) {
        const move = button(glyph, () => { const next = [...items]; [next[index], next[index + delta]] = [next[index + delta], next[index]]; this.update({ ...this.draft.preferences, layout: { ...this.draft.preferences.layout, [section]: next } }); }, `Move ${label} ${delta < 0 ? "earlier" : "later"}`); move.disabled = index + delta < 0 || index + delta >= items.length; move.dataset.settingsControl = `${id}:move:${delta}`; row.append(move);
      }
      const remove = button("×", () => this.update({ ...this.draft.preferences, layout: { ...this.draft.preferences.layout, [section]: items.filter((item) => item !== id) } }), `Remove ${label}`); remove.dataset.settingsControl = `${id}:remove`; row.append(remove); list.append(row);
    });
    if (!items.length) body.append(el("p", "keyboard-settings__note", "No keys yet. Add a key or apply a preset."));
    body.append(list);
  }
  private renderPrefixes(body: HTMLElement): void {
    body.append(el("p", "keyboard-settings__note", "Advanced: a sequence sends a prefix key, then another key, to the program inside the terminal. This can control a nested tmux or screen app, but not Persea’s surrounding tmux session. Use Keys for ordinary keys and the session menu to switch sessions."));
    for (const [name, id] of Object.entries(this.draft.preferences.prefixes)) {
      const row = el("div", "keyboard-settings__row"); row.append(el("span", "keyboard-settings__key-name", `${name} · ${this.label(id)}`), button("Change", () => { this.editingPrefix = name; this.catalog = true; this.group = "Letters"; this.modifiers = NO_MODIFIERS; this.render(); }, `Change ${name} prefix`), button("×", () => { const prefixes = { ...this.draft.preferences.prefixes }; delete prefixes[name]; this.update({ ...this.draft.preferences, prefixes }); }, `Remove ${name} prefix`)); body.append(row);
    }
    const add = el("div", "keyboard-settings__list-tools"), input = el("input"); input.type = "text"; input.maxLength = 32; input.placeholder = "Prefix name"; input.setAttribute("aria-label", "New prefix name"); input.dataset.settingsControl = "prefix-name"; input.value = this.prefixName; input.addEventListener("input", () => { this.prefixName = input.value; });
    add.append(input, button("Add prefix", () => {
      const name = this.prefixName.trim();
      if (!validPrefixName(name)) { this.status = "Use a name starting with a letter, up to 32 letters, digits, spaces, _ or -."; this.render(); return; }
      if (Object.keys(this.draft.preferences.prefixes).some((existing) => existing.toLowerCase() === name.toLowerCase())) { this.status = "That prefix name already exists. Use Change beside it to edit its key."; this.render(); return; }
      if (Object.keys(this.draft.preferences.prefixes).length >= 20) { this.status = "You can save up to 20 prefixes."; this.render(); return; }
      this.editingPrefix = name; this.catalog = true; this.group = "Letters"; this.render();
    })); body.append(add);
    if (this.catalog) this.renderCatalog(body);
  }
  private renderCatalog(body: HTMLElement): void {
    const catalog = el("section", "keyboard-settings__catalog"); catalog.setAttribute("aria-label", "Choose a key");
    if (this.editingPrefix) catalog.append(el("p", "", `Choose the prefix key for ${this.editingPrefix}.`));
    const controls = el("div", "keyboard-settings__catalog-controls"); const groups = el("select"); groups.setAttribute("aria-label", "Key group");
    groups.dataset.settingsControl = "catalog-group";
    for (const name of [...Object.keys(KEY_GROUPS), ...(!this.editingPrefix ? ["Sequences", "Modifiers"] : [])]) { const option = el("option", "", name === "Function" ? "Fn · F1–F12" : name); option.value = name; groups.append(option); }
    if (!Array.from(groups.options).some((option) => option.value === this.group)) this.group = "Navigation";
    groups.value = this.group; groups.addEventListener("change", () => { this.group = groups.value; this.render(); }); controls.append(groups);
    for (const modifier of ["ctrl", "alt", "shift"] as const) { const control = button(modifier === "shift" ? "⇧" : modifier === "ctrl" ? "Ctrl" : "Alt", () => { this.modifiers = { ...this.modifiers, [modifier]: !this.modifiers[modifier] }; this.render(); }, `${modifier} modifier`); control.setAttribute("aria-pressed", String(this.modifiers[modifier])); control.className = "keyboard-settings__modifier"; controls.append(control); }
    catalog.append(controls);
    if (this.group === "Sequences") {
      const select = el("select"); select.setAttribute("aria-label", "Sequence prefix"); select.dataset.settingsControl = "catalog-prefix"; for (const name of Object.keys(this.draft.preferences.prefixes)) { const option = el("option", "", name); option.value = name; select.append(option); } if (!this.draft.preferences.prefixes[this.prefix]) this.prefix = select.options[0]?.value ?? ""; select.value = this.prefix; select.addEventListener("change", () => { this.prefix = select.value; this.render(); }); catalog.append(select);
    }
    const grid = el("div", "keyboard-settings__catalog-grid");
    const addID = (id: string) => {
      if (this.editingPrefix) { const name = this.editingPrefix; this.catalog = false; this.editingPrefix = ""; this.prefixName = ""; this.nextFocus = `Change ${name} prefix`; this.update({ ...this.draft.preferences, prefixes: { ...this.draft.preferences.prefixes, [name]: id } }); }
      else { const section = this.section as "bar" | "favorites", items = this.draft.preferences.layout[section]; if (items.length >= 100 || items.includes(id)) return; this.update({ ...this.draft.preferences, layout: { ...this.draft.preferences.layout, [section]: [...items, id] } }); }
    };
    if (this.group === "Modifiers") { for (const modifier of ["ctrl", "alt", "shift"]) grid.append(button(this.label(`modifier:${modifier}`), () => addID(`modifier:${modifier}`), `Add ${modifier} modifier button`)); }
    else for (const key of this.group === "Sequences" ? [...KEY_GROUPS.Letters, ...KEY_GROUPS.Navigation] : KEY_GROUPS[this.group] ?? []) {
      const entry = this.group === "Sequences" ? prefixSequence(this.prefix, { key, modifiers: this.modifiers }) : keyAction(key, this.modifiers);
      const reason = planKey({ key, modifiers: this.modifiers }); const control = button(keyGlyph(key), () => addID(entry.id), `Add ${entry.label}${typeof reason === "string" ? ": " + reason : ""}`);
      control.disabled = typeof reason === "string" || (this.group === "Sequences" && !this.prefix);
      grid.append(control);
    }
    catalog.append(grid); body.append(catalog);
  }
}
let settingsSerial = 0;
