// The web UI's one icon surface: a deliberately small Lucide SVG subset. Named
// imports let esbuild tree-shake the other ~1,600 icons, while inline SVG avoids a
// font request, a private-use glyph fallback, and screen-reader pronunciation.
// Lucide 1.25.0 is ISC licensed (some inherited Feather icons are MIT); the exact
// upstream notice is committed at web/src/licenses/lucide/LICENSE.

import {
  Archive,
  ArchiveRestore,
  ArrowRight,
  Bell,
  Bot,
  Check,
  ChevronDown,
  Circle,
  CircleQuestionMark,
  CircleDashed,
  Diamond,
  Ellipsis,
  ExternalLink,
  FileText,
  Folder,
  FolderGit2,
  Funnel,
  GitBranch,
  Link,
  LoaderCircle,
  Menu,
  OctagonX,
  PanelsTopLeft,
  Pencil,
  Play,
  Plus,
  RefreshCw,
  Square,
  SquareCheckBig,
  Terminal,
  Trash2,
  TriangleAlert,
  X,
  type IconNode,
} from "lucide";

const ICONS = {
  archive: Archive,
  "archive-restore": ArchiveRestore,
  "arrow-right": ArrowRight,
  bell: Bell,
  bot: Bot,
  check: Check,
  "chevron-down": ChevronDown,
  circle: Circle,
  "circle-dashed": CircleDashed,
  "circle-question": CircleQuestionMark,
  diamond: Diamond,
  ellipsis: Ellipsis,
  "external-link": ExternalLink,
  folder: Folder,
  "folder-git": FolderGit2,
  "file-text": FileText,
  funnel: Funnel,
  "git-branch": GitBranch,
  link: Link,
  "loader-circle": LoaderCircle,
  menu: Menu,
  "octagon-x": OctagonX,
  panels: PanelsTopLeft,
  pencil: Pencil,
  play: Play,
  plus: Plus,
  reload: RefreshCw,
  square: Square,
  "square-check": SquareCheckBig,
  terminal: Terminal,
  trash: Trash2,
  "triangle-alert": TriangleAlert,
  x: X,
} as const satisfies Record<string, IconNode>;

export type IconName = keyof typeof ICONS;

/** Builds a decorative inline SVG that inherits the surrounding currentColor.
 * Meaning stays on the adjacent text or the containing control's accessible name;
 * callers cannot accidentally expose a private icon name to assistive technology. */
export function icon(name: IconName, className = ""): SVGSVGElement {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", `af-icon${className ? ` ${className}` : ""}`);
  svg.setAttribute("data-icon", name);
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "2");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");

  for (const [tag, attrs] of ICONS[name]) {
    const child = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [attr, value] of Object.entries(attrs)) {
      child.setAttribute(attr, String(value));
    }
    svg.append(child);
  }
  return svg;
}
