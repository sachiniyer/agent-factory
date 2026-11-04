// The syntax grammars for fenced code blocks in the Docs view (#6731), built
// into their own lazy chunk, dist/lazy/codeblocks.js — never the eager shell,
// never the Docs bundle itself, and only fetched when a previewed doc holds a
// fenced block whose info string names a language. Code is only ever text:
// this module returns text ranges and class names, never markup.
//
// The language set is curated — the grammars a proposal doc is likely to
// carry — rather than every language CodeMirror ships, because the chunk's
// size is a deliberate budget (see scripts/perf/baselines.json). Go, shell,
// TOML and diff come through the legacy stream modes because there is no
// Lezer grammar for them.

import type { Language } from "@codemirror/language";
import { StreamLanguage } from "@codemirror/language";
import { javascriptLanguage, jsxLanguage, tsxLanguage, typescriptLanguage } from "@codemirror/lang-javascript";
import { pythonLanguage } from "@codemirror/lang-python";
import { cssLanguage } from "@codemirror/lang-css";
import { htmlLanguage } from "@codemirror/lang-html";
import { jsonLanguage } from "@codemirror/lang-json";
import { markdownLanguage } from "@codemirror/lang-markdown";
import { rustLanguage } from "@codemirror/lang-rust";
import { StandardSQL } from "@codemirror/lang-sql";
import { yamlLanguage } from "@codemirror/lang-yaml";
import { go } from "@codemirror/legacy-modes/mode/go";
import { shell } from "@codemirror/legacy-modes/mode/shell";
import { toml } from "@codemirror/legacy-modes/mode/toml";
import { diff } from "@codemirror/legacy-modes/mode/diff";
import { classHighlighter, highlightTree, tagHighlighter, tags } from "@lezer/highlight";

const goLanguage = StreamLanguage.define(go);
const shellLanguage = StreamLanguage.define(shell);
const tomlLanguage = StreamLanguage.define(toml);
const diffLanguage = StreamLanguage.define(diff);

/** Alias → grammar, normalised to lowercase before lookup. Aliases are the
 *  names people actually write after the ```: go/golang, js/node, tsx, bash.
 *  A Map, not an object literal: a document names the key, so an info string
 *  like "constructor" or "__proto__" must resolve to nothing — through an
 *  object it would hit Object.prototype and come back truthy-but-not-a-
 *  grammar, an error the "unknown means plain code" contract forbids. */
const LANGS = new Map<string, Language>([
  ["go", goLanguage],
  ["golang", goLanguage],
  ["js", javascriptLanguage],
  ["javascript", javascriptLanguage],
  ["mjs", javascriptLanguage],
  ["cjs", javascriptLanguage],
  ["node", javascriptLanguage],
  ["jsx", jsxLanguage],
  ["ts", typescriptLanguage],
  ["typescript", typescriptLanguage],
  ["mts", typescriptLanguage],
  ["cts", typescriptLanguage],
  ["tsx", tsxLanguage],
  ["py", pythonLanguage],
  ["python", pythonLanguage],
  ["python3", pythonLanguage],
  ["sh", shellLanguage],
  ["bash", shellLanguage],
  ["zsh", shellLanguage],
  ["shell", shellLanguage],
  ["console", shellLanguage],
  ["json", jsonLanguage],
  ["jsonc", jsonLanguage],
  ["yaml", yamlLanguage],
  ["yml", yamlLanguage],
  ["toml", tomlLanguage],
  ["html", htmlLanguage],
  ["htm", htmlLanguage],
  ["css", cssLanguage],
  ["sql", StandardSQL.language],
  ["rs", rustLanguage],
  ["rust", rustLanguage],
  ["diff", diffLanguage],
  ["patch", diffLanguage],
  ["md", markdownLanguage],
  ["markdown", markdownLanguage],
]);

// classHighlighter maps a tag to the tok-<name> of its nearest mapped
// ancestor, which leaves function calls, builtins, constants, quotes and
// diff-changed lines on the plain-text path. These extras name them; both
// highlighters run together and the emitted class list is the union.
const extraClasses = tagHighlighter([
  { tag: tags.function(tags.variableName), class: "tok-function" },
  { tag: tags.function(tags.propertyName), class: "tok-function" },
  { tag: tags.standard(tags.variableName), class: "tok-builtin" },
  { tag: tags.constant(tags.variableName), class: "tok-constant" },
  { tag: tags.quote, class: "tok-quote" },
  { tag: tags.strikethrough, class: "tok-strikethrough" },
  { tag: tags.changed, class: "tok-changed" },
]);

// The work is bounded on a huge block: above this the widget paints plain
// code instantly rather than spending frames tokenising and building spans.
export const MAX_HIGHLIGHT_CHARS = 64 * 1024;

export interface HighlightSpan {
  from: number;
  to: number;
  cls: string;
}

/** The grammar `lang` resolves to, or null for an unknown/missing language —
 *  a block like that renders as plain code, never an error. */
export function languageFor(lang: string): Language | null {
  return LANGS.get(lang.trim().toLowerCase()) ?? null;
}

/** Token-coloured ranges over `source` for `lang` — ordered, non-overlapping,
 *  adjacent same-class runs merged. Null when the language is unknown or the
 *  source exceeds the highlight bound. */
export function highlightRanges(source: string, lang: string): HighlightSpan[] | null {
  const language = languageFor(lang);
  if (!language || source.length > MAX_HIGHLIGHT_CHARS) return null;
  const spans: HighlightSpan[] = [];
  highlightTree(language.parser.parse(source), [classHighlighter, extraClasses], (from, to, classes) => {
    const last = spans[spans.length - 1];
    if (last && last.to === from && last.cls === classes) {
      last.to = to;
    } else {
      spans.push({ from, to, cls: classes });
    }
  });
  return spans;
}
