import { bindTapActivation } from "./tap_activation";
import { actionGlyph, KEY_GROUPS, NO_MODIFIERS, chordLabel, keyAction, keyEncodingNote, keyGlyph, planKey, resolveAction, type KeyModifiers, type TerminalActionEntry } from "./terminal_actions";
import { documentKeyboardPreferences, type KeyboardPreferences, type KeyboardPreferencesService } from "./keyboard_preferences";
import { KeyboardSettings, keyboardIcon } from "./keyboard_settings";

type PanelOptions = Readonly<{
  generation(): number; isLive(): boolean; dispatch(entry: TerminalActionEntry, event: Event): string;
  changed(): void; beforeChange(): void; preferencesChanged(): void;
  hideKeyboard(): void; keyboardOpen(): boolean; settingsReturnFocus(): HTMLElement;
  preferences?: KeyboardPreferencesService;
}>;

type Picker = "key-group";
let nextPickerId = 0;

/** Input-only disclosure. Its buttons preserve native text focus. Editing
 * settings has its own modal and never shares an action callback with keys. */
export class TerminalKeysPanel {
  readonly element = document.createElement("section");
  private readonly service: KeyboardPreferencesService;
  private readonly settings: KeyboardSettings;
  private readonly unsubscribe: () => void;
  private view: "Favorites" | "All keys" | "Fn" = "Favorites";
  private group = "Navigation";
  private modifiers: KeyModifiers = NO_MODIFIERS;
  private bindings: (() => void)[] = [];
  private revision = 0;
  private message = "";
  private picker: Picker | undefined;
  private readonly pickerId = `persea-key-choices-${++nextPickerId}`;
  constructor(private readonly options: PanelOptions) {
    this.service = options.preferences ?? documentKeyboardPreferences();
    this.settings = new KeyboardSettings(this.service);
    let preferenceSignature = JSON.stringify(this.service.snapshot().effective);
    this.unsubscribe = this.service.subscribe((snapshot) => {
      const signature = JSON.stringify(snapshot.effective);
      if (signature === preferenceSignature) return;
      preferenceSignature = signature;
      this.revision++; this.modifiers = NO_MODIFIERS;
      if (this.open) this.render(); this.options.preferencesChanged(); this.options.changed();
    });
    this.element.className = "persea-terminal-keys";
    this.element.setAttribute("aria-label", "Terminal Keys"); this.element.hidden = true;
    this.element.addEventListener("pointerdown", (event) => {
      if (!event.isTrusted || event.button !== 0) return;
      const target = event.target instanceof Element ? event.target : undefined;
      if (target?.closest("button, input, textarea, select, a, [contenteditable]")) return;
      // The space between controls is still part of the keyboard. Cancelling
      // pointerdown's mouse-focus default keeps the native editing transaction;
      // touch panning remains governed by touch-action and can scroll normally.
      if (document.activeElement instanceof HTMLTextAreaElement && !this.element.contains(document.activeElement)) event.preventDefault();
    });
    this.element.addEventListener("pointercancel", () => this.cancelModifiers());
    this.element.addEventListener("keydown", (event) => {
      if (!event.isTrusted || event.key !== "Escape" || !this.picker) return;
      // Only a key directed at this disclosure belongs to it. A hardware
      // Escape in the still-focused terminal remains terminal input.
      event.preventDefault(); event.stopPropagation();
      this.picker = undefined; this.render(); this.options.changed();
    });
  }
  get open(): boolean { return !this.element.hidden; }
  get preferences(): KeyboardPreferences { return this.service.snapshot().effective; }
  get ctrlLatched(): boolean { return this.modifiers.ctrl; }
  get hasModifiers(): boolean { return Object.values(this.modifiers).some(Boolean); }
  isModifierLatched(modifier: keyof KeyModifiers): boolean { return this.modifiers[modifier]; }
  toggle(): void { this.setOpen(!this.open); }
  setOpen(open: boolean): void {
    if (open === this.open) return;
    this.options.beforeChange(); this.revision++; this.modifiers = NO_MODIFIERS; this.message = ""; this.picker = undefined;
    this.element.hidden = !open;
    if (open) { this.view = "Favorites"; this.group = "Navigation"; this.render(); }
    else { this.disposeBindings(); this.element.replaceChildren(); }
    this.options.changed();
  }
  showAll(group = "Navigation", ctrl = false): void {
    this.setOpen(true); this.view = group === "Function" ? "Fn" : "All keys"; this.group = KEY_GROUPS[group] ? group : "Navigation";
    this.modifiers = { ...NO_MODIFIERS, ctrl }; this.message = ""; this.picker = undefined; this.render(); this.options.changed();
  }
  toggleModifier(modifier: keyof KeyModifiers): void {
    if (this.open && this.view === "Favorites") { this.view = "All keys"; this.group = "Letters"; }
    this.modifiers = { ...this.modifiers, [modifier]: !this.modifiers[modifier] }; this.message = ""; this.picker = undefined; this.render(); this.options.changed();
  }
  openSettings(): void { this.cancel(); this.settings.open(this.options.settingsReturnFocus()); }
  activateBar(entry: TerminalActionEntry, event: Event): TerminalActionEntry {
    if (entry.action.kind === "key") {
      const chord = entry.action.chord;
      entry = keyAction(chord.key, { ctrl: chord.modifiers.ctrl || this.modifiers.ctrl, alt: chord.modifiers.alt || this.modifiers.alt, shift: chord.modifiers.shift || this.modifiers.shift });
    }
    this.activate(entry, event);
    return entry;
  }
  keyboardChanged(): void { if (this.open) { this.render(); this.options.changed(); } }
  disarmForNativeInput(): void { this.modifiers = NO_MODIFIERS; this.revision++; }
  takeNativeCharacter(character: string): TerminalActionEntry | undefined {
    if (!this.hasModifiers || !/^[a-zA-Z]$/.test(character)) return;
    const entry = keyAction(character.toLowerCase(), { ...this.modifiers, shift: this.modifiers.shift || character !== character.toLowerCase() });
    this.disarmForNativeInput();
    return entry;
  }
  cancel(): void { this.setOpen(false); this.cancelModifiers(); this.revision++; }
  cancelModifiers(): void {
    if (!Object.values(this.modifiers).some(Boolean)) return;
    this.modifiers = NO_MODIFIERS; this.revision++; this.message = "";
    if (this.open) this.render(); this.options.changed();
  }
  private disposeBindings(): void { this.bindings.splice(0).forEach((dispose) => dispose()); }
  dispose(): void { this.unsubscribe(); this.settings.dispose(); this.cancel(); this.disposeBindings(); }
  private button(label: string, id: string, action: (event: Event) => void, title = label): HTMLButtonElement {
    const button = document.createElement("button"); button.type = "button"; button.textContent = label; button.title = title; button.setAttribute("aria-label", title); button.dataset.keyControl = id;
    if (/^(?:key:|sequence:)/.test(id)) button.dataset.keyFocus = id.replace(/:[0-7]$/, "");
    this.bindings.push(bindTapActivation(button, action, () => {}, () => this.open && this.options.isLive(), () => this.options.generation() + this.revision));
    return button;
  }
  private activate(entry: TerminalActionEntry, event: Event): void {
    this.message = this.options.dispatch(entry, event); this.modifiers = NO_MODIFIERS; this.render(); this.options.changed();
  }
  private actionButton(entry: TerminalActionEntry, bare = false): HTMLButtonElement {
    const chord = entry.action.kind === "key" ? entry.action.chord : undefined;
    const reason = chord ? planKey(chord) : entry.action.kind === "sequence" && !this.preferences.prefixes[entry.action.prefix] ? "Configure this sequence's prefix in Keyboard settings." : undefined;
    const note = chord ? keyEncodingNote(chord) : "Sends the configured prefix and key to the program inside this terminal.";
    const label = bare && chord ? keyGlyph(chord.key) : actionGlyph(entry);
    const title = typeof reason === "string" ? `${entry.label}: ${reason}` : `${entry.label}${note ? ". " + note : ""}`;
    const button = this.button(label, entry.id, (event) => {
      if (typeof reason === "string") { this.message = reason; this.render(); this.options.changed(); return; }
      this.activate(entry, event);
    }, title);
    button.dataset.keyKind = chord && chord.key.length > 1 ? "special" : "character";
    if (typeof reason === "string") button.setAttribute("aria-disabled", "true");
    return button;
  }
  private pickerButton(picker: Picker): HTMLButtonElement {
    const expanded = this.picker === picker;
    const value = this.group;
    const button = this.button("", picker, () => {
      this.picker = expanded ? undefined : picker;
      this.message = ""; this.render(); this.options.changed();
    }, `Key group: ${value}`);
    button.className = "persea-terminal-keys__picker-trigger";
    button.setAttribute("aria-expanded", String(expanded));
    if (expanded) button.setAttribute("aria-controls", this.pickerId);
    const label = document.createElement("span");
    label.className = "persea-terminal-keys__picker-label";
    label.textContent = expanded ? "Key groups" : value;
    const arrow = document.createElement("span"); arrow.setAttribute("aria-hidden", "true"); arrow.textContent = expanded ? "‹" : "▾";
    if (expanded) button.append(arrow, label); else button.append(label, arrow);
    return button;
  }
  private pickerChoice(value: string, label: string): HTMLButtonElement {
    const picker = this.picker!;
    const button = this.button(label, `choose-group:${value}`, () => {
      this.group = value;
      this.picker = undefined; this.message = ""; this.render(); this.options.changed();
    });
    button.dataset.keyReturnFocus = picker;
    button.setAttribute("aria-pressed", String(value === this.group));
    return button;
  }
  private render(): void {
    if (!this.open) return;
    // Changing the catalog can change its intrinsic height even when the
    // disclosure and height cap stay open. Capture before CSS or content moves.
    this.options.beforeChange();
    this.element.dataset.view = this.view; this.element.dataset.group = this.group;
    this.element.dataset.picker = this.picker ?? "";
    const focusedElement = this.element.contains(document.activeElement) ? document.activeElement as HTMLElement : undefined;
    const focused = focusedElement?.dataset.keyControl, focusedKey = focusedElement?.dataset.keyFocus, returnFocus = focusedElement?.dataset.keyReturnFocus;
    this.disposeBindings(); this.revision++;
    const nav = document.createElement("div"); nav.className = "persea-terminal-keys__nav";
    for (const view of ["Favorites", "All keys", "Fn"] as const) {
      const button = this.button(view, `view:${view}`, () => {
        this.view = view; if (view === "Fn") this.group = "Function"; else if (this.group === "Function") this.group = "Navigation";
        this.message = ""; this.modifiers = NO_MODIFIERS; this.picker = undefined; this.render(); this.options.changed();
      }, view === "Fn" ? "Function keys F1–F12" : view);
      button.setAttribute("aria-pressed", String(this.view === view)); nav.append(button);
    }
    const settings = this.button("", "customize", () => this.openSettings(), "Keyboard settings"); settings.className = "persea-terminal-keys__utility"; settings.append(keyboardIcon("settings")); nav.append(settings);
    if (this.options.keyboardOpen()) {
      const hide = this.button("", "hide-keyboard", () => { this.cancel(); this.options.hideKeyboard(); }, "Hide phone keyboard"); hide.className = "persea-terminal-keys__utility"; hide.append(keyboardIcon("hide")); nav.append(hide);
    }
    const body = document.createElement("div"); body.className = "persea-terminal-keys__body";
    if (this.picker) {
      const navigation = document.createElement("div"); navigation.className = "persea-terminal-keys__options";
      navigation.append(this.pickerButton(this.picker)); body.append(navigation);
    } else if (this.view !== "Favorites") {
      const modifiers = document.createElement("div"); modifiers.className = "persea-terminal-keys__options";
      if (this.view === "All keys") {
        modifiers.append(this.pickerButton("key-group"));
      }
      for (const modifier of ["ctrl", "alt", "shift"] as const) {
        const button = this.button(modifier === "shift" ? "⇧" : modifier === "ctrl" ? "Ctrl" : "Alt", `modifier:${modifier}`, () => this.toggleModifier(modifier), modifier === "shift" ? "Shift" : modifier === "ctrl" ? "Ctrl" : "Alt");
        button.className = "persea-terminal-keys__modifier"; button.setAttribute("aria-pressed", String(this.modifiers[modifier])); modifiers.append(button);
      }
      body.append(modifiers);
    }
    const grid = document.createElement("div"); grid.className = "persea-terminal-keys__grid";
    if (this.picker) {
      grid.id = this.pickerId; grid.classList.add("persea-terminal-keys__choices"); grid.setAttribute("role", "group");
      grid.setAttribute("aria-label", "Key groups");
      for (const group of Object.keys(KEY_GROUPS).filter((name) => name !== "Function")) grid.append(this.pickerChoice(group, group));
    } else if (this.view === "Favorites") {
      for (const id of this.preferences.layout.favorites) {
        if (id.startsWith("modifier:")) {
          const modifier = id.slice(9) as keyof KeyModifiers;
          const button = this.button(modifier === "shift" ? "⇧" : modifier === "ctrl" ? "Ctrl" : "Alt", id, () => this.toggleModifier(modifier));
          button.setAttribute("aria-pressed", String(this.modifiers[modifier])); grid.append(button);
        } else { const entry = resolveAction(id); if (entry) grid.append(this.actionButton(entry)); }
      }
      if (!grid.children.length) { const empty = document.createElement("p"); empty.textContent = "Add favorites in Keyboard settings."; grid.append(empty); }
    } else for (const key of KEY_GROUPS[this.group] ?? []) grid.append(this.actionButton(keyAction(key, this.modifiers), true));
    body.append(grid);
    const status = document.createElement("div"); status.className = "persea-terminal-keys__status"; status.setAttribute("role", "status");
    const armed = Object.values(this.modifiers).some(Boolean);
    status.textContent = this.message || (armed ? `Next key: ${chordLabel({ key: "…", modifiers: this.modifiers })}. Tap a selected modifier to cancel.` : "");
    status.dataset.announcementOnly = String(!this.message || this.message.endsWith(" sent"));
    this.element.replaceChildren(nav, body, status);
    if (focused) {
      const controls = Array.from(this.element.querySelectorAll<HTMLElement>("[data-key-control]"));
      (controls.find((control) => control.dataset.keyControl === focused) ?? (returnFocus ? controls.find((control) => control.dataset.keyControl === returnFocus) : undefined) ?? (focusedKey ? controls.find((control) => control.dataset.keyFocus === focusedKey) : undefined) ?? controls.find((control) => control.dataset.keyControl === `view:${this.view}`) ?? controls[0])?.focus({ preventScroll: true });
    }
  }
}
