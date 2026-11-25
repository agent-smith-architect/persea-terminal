// Class-based miniature layout diagram for split trees.
// Pure DOM helper — zero inline style attributes, zero transport imports.
import "./workspace_diagram.css";
import { normalizeWeights, sessionKey, type NodePath, type WorkspaceLeaf, type WorkspaceNode } from "./workspace_model";

export type WorkspaceDiagramOptions = Readonly<{
  interactive?: boolean;
  showLabels?: boolean;
  selectedKey?: string;
  addedKeys?: ReadonlySet<string>;
  labelFor?: (leaf: WorkspaceLeaf) => string;
  availabilityFor?: (leaf: WorkspaceLeaf) => "ready" | "missing" | "blocked" | undefined;
  onSelect?: (key: string, path: NodePath, leaf: WorkspaceLeaf) => void;
  ariaLabel?: string;
}>;

function clampedWeightClass(weight: number): string {
  const clamped = Math.max(1, Math.min(100, Math.round(weight)));
  return `ws-w-${clamped}`;
}

export function renderWorkspaceDiagram(tree: WorkspaceNode, options: WorkspaceDiagramOptions = {}): HTMLElement {
  const interactive = options.interactive === true;
  const showLabels = options.showLabels === true;
  const root = document.createElement("div");
  root.className = interactive ? "ws-mini ws-mini--interactive" : "ws-mini ws-mini--decorative";
  if (interactive) {
    root.setAttribute("role", "group");
    root.setAttribute("aria-label", options.ariaLabel ?? "Workspace layout preview");
  } else {
    root.setAttribute("aria-hidden", "true");
  }

  const build = (node: WorkspaceNode, path: NodePath, weightClass?: string): HTMLElement => {
    const track = document.createElement("div");
    track.className = weightClass ? `ws-mini__track ${weightClass}` : "ws-mini__track ws-w-100";
    if (node.kind === "leaf") {
      const key = sessionKey(node.session);
      const labelText = options.labelFor ? options.labelFor(node) : (node.aliasHint ?? node.session.name);
      const cell = interactive ? document.createElement("button") : document.createElement("span");
      cell.className = interactive ? "ws-mini__leaf ws-mini__leaf--interactive" : "ws-mini__leaf";
      if (cell instanceof HTMLButtonElement) {
        cell.type = "button";
        cell.setAttribute("aria-pressed", String(options.selectedKey === key));
        cell.setAttribute("aria-label", `Select pane ${labelText}`);
        cell.addEventListener("click", () => options.onSelect?.(key, path, node));
      }
      cell.dataset.wsMiniSession = node.session.name;
      if (options.selectedKey === key) cell.dataset.wsSelected = "true";
      if (options.addedKeys?.has(key)) cell.dataset.wsStaged = "added";
      const availability = options.availabilityFor?.(node);
      if (availability) cell.dataset.wsAvailability = availability;
      if (showLabels) {
        const span = document.createElement("span");
        span.className = "ws-mini__leaf-label";
        span.textContent = labelText;
        cell.append(span);
        cell.title = labelText;
      }
      track.append(cell);
      return track;
    }
    const container = document.createElement("div");
    container.className = `ws-mini__split ws-mini__split--${node.direction}`;
    const weights = normalizeWeights(node.weights);
    node.children.forEach((child, index) => {
      container.append(build(child, [...path, index], clampedWeightClass(weights[index] ?? 1)));
    });
    track.append(container);
    return track;
  };

  root.append(build(tree, []));
  return root;
}
