# Design — MCPaw

A locked design system for the MCPaw admin console. Every page redesign reads
this file before emitting code. Do not regenerate per page — extend or amend
this file when the system needs to grow.

## Genre

**modern-minimal.** MCPaw is infrastructure: an API-to-MCP gateway with tokens,
egress policy and an audit trail. The console should read like an instrument
panel — calm, precise, fast — not like a marketing template.

## Theme

**Cobalt.** Cool engineered near-white paper, one electric cobalt signal, ruler-drawn
hairlines, tight technical radii. The accent is a *signal*, never a flood: it marks
the active nav item, the focus ring, the one primary button per view, and a live
status. It occupies well under 5% of any viewport.

| Token | Light | Dark |
| --- | --- | --- |
| `--color-paper` | `oklch(98.5% 0.004 250)` | `oklch(16% 0.012 258)` |
| `--color-paper-2` | `oklch(96.4% 0.005 252)` | `oklch(19.5% 0.013 258)` |
| `--color-paper-3` | `oklch(93.8% 0.006 252)` | `oklch(23% 0.014 258)` |
| `--color-graphite` | `oklch(22% 0.016 260)` | `oklch(13% 0.014 260)` |
| `--color-rule` | `oklch(89% 0.008 252)` | `oklch(29% 0.014 258)` |
| `--color-rule-2` | `oklch(80% 0.010 252)` | `oklch(38% 0.016 258)` |
| `--color-muted` | `oklch(52% 0.016 256)` | `oklch(68% 0.014 256)` |
| `--color-ink-2` | `oklch(34% 0.018 257)` | `oklch(86% 0.010 256)` |
| `--color-ink` | `oklch(24% 0.020 258)` | `oklch(95% 0.008 256)` |
| `--color-accent` | `oklch(50% 0.20 256)` | `oklch(72% 0.16 256)` |
| `--color-focus` | `oklch(50% 0.20 256)` | `oklch(76% 0.15 256)` |

Status hues are semantic, not decorative, and each pairs colour with a word — never
colour alone: `--color-ok` (green 150), `--color-warn` (amber 75), `--color-danger`
(red 25). All are cool-tinted toward the anchor and all carry chroma; no flat greys
anywhere in the palette.

Dark mode keeps the anchor hue and moves only lightness and chroma, per the recipe:
paper rises to 16 %, ink falls to 95 %, accent gains lightness and sheds chroma, and
higher surfaces are *lighter*, not darker.

## Typography

Three families — the 2+1 ceiling, no more.

- **Display:** Space Grotesk 500/600, tracking `-0.02em`. Page titles, card titles, section heads.
- **Body:** Inter 400/500/600. Prose, form labels, table cells.
- **Outlier (mono):** JetBrains Mono 400/500. Two roles only — machine-readable values
  (slugs, endpoints, tool names, IDs, tokens) and small uppercase meta labels at
  `0.06em` tracking. Never body copy.

All three are **self-hosted** as latin-subset variable woff2 under
`/static/fonts/`. The console runs under a `default-src 'none'` CSP whose
`font-src` is `'self'`, so a CDN link would be blocked — and an infrastructure
console should not call a third party on every page load. See
`internal/webui/static/fonts/README.md` for licensing (all three are OFL 1.1).

Type scale is a major third (1.25) anchored at 15px body. Headings are **roman
only** — no italic display, ever. Measure caps at `68ch`.

Any container showing a column of numbers or timestamps sets
`font-variant-numeric: tabular-nums`.

## Spacing

4-point named scale, nine steps, in `tokens.css`. Pages use named tokens
(`var(--space-lg)`), never raw values. Section rhythm is deliberately uneven:
panels tighten (`--space-lg`) where they are dense and open (`--space-xl`) where
they carry prose.

## Macrostructure families

Pages within a family share the family's shape and vary only in component
archetypes.

- **Index pages** (instances · connectors · tokens · audit) — **Catalogue**.
  A uniform survey of variations of one thing. Card grid where each item has
  status worth seeing at a glance (instances, connectors); dense hairline table
  where the value is scanning many rows (tokens, audit).
- **Working pages** (instance detail) — **Workbench**. The page is the surface you
  operate: stacked hairline panels, each one a task, in the order an operator
  actually needs them (endpoint → configuration → credentials → tools → delete).
  No screenshots: this *is* the app, and re-drawn chrome is banned.
- **Focused task pages** (login · setup) — single narrow column, no nav, no footer
  links. One job, one form, one button.

## Motion

This is an app, not a landing page. The page is composed; it does not perform.

- Easings: `--ease-out: cubic-bezier(0.16, 1, 0.3, 1)`, `--ease-in-out: cubic-bezier(0.65, 0, 0.35, 1)`.
- **No scroll reveals.** Content is simply there.
- Permitted motion, and nothing else: the command palette opening
  (opacity + transform), hairline border-colour shifts on hover, and the 1px
  press translate on buttons.
- Focus rings appear **instantly** — never transitioned.
- `prefers-reduced-motion: reduce` collapses everything to ≤ 120ms opacity.

## Microinteractions stance

- **Silent success.** A saved form redirects and shows the saved state. No
  celebratory toasts.
- **Confirmation only for the irreversible.** Deleting an instance destroys stored
  credentials and every token scoped to it, so it demands typing the slug. Nothing
  else asks "are you sure".
- Hover affordances always have a focus equivalent; nothing is hover-only.
- The ⌘K palette is a real dialog: focus-trapped, Esc closes, ↑/↓ navigate,
  Enter opens, body scroll locked.

## CTA voice

- **Primary** — solid cobalt, 6px radius, one per view. Names the destination or
  the effect: "Create instance", "Issue token", "Save configuration".
- **Secondary** — hairline-bordered, paper-2 fill, 6px radius.
- **Destructive** — hairline-bordered in danger, filling only on hover. Destructive
  actions never lead with a filled red button.
- **Never** a pill, a gradient, or "Click here". Never a label that wraps to two lines.

## Per-page allowances

- App pages **must not** use enrichment. Function carries the page.
- The **graphite surface** is reserved for content that is genuinely machine-facing:
  the MCP endpoint URL, a one-time token reveal, a response preview, and the command
  palette. It is never decoration, and it never wears fake window chrome.
- Icons: none from a library. The only mark is the hand-built wordmark glyph.
  No emoji anywhere in the interface.

## What pages MUST share

- The wordmark and its hand-drawn paw mark.
- The accent colour and its placement (< 5% per viewport).
- The three font families and their roles.
- The CTA voice — shape, radius, padding rhythm.
- Hairline structure: 1px `--color-rule` defines every surface. No drop shadows
  beyond the single `0 1px 2px` lift on the palette panel. No boxed card-in-card.

## What pages MAY differ on

- Macrostructure within the family (card Catalogue vs tabular Catalogue).
- Panel density and section padding.
- Whether a page carries a graphite surface at all.

## Copy

Declarative, technical, specific. Name the endpoint, the command, the number.
Curly quotes, em-dashes, real ellipses. Never *seamless, robust, cutting-edge,
leverage, revolutionary, unlock, supercharge*. Never invent a metric — if a number
is not known, the interface says so.

## Exports

### tokens.css

The canonical token block lives at `internal/webui/static/tokens.css` and is
imported by `app.css`. Every colour and every `font-family` in the stylesheet
references a token by name; no inline OKLCH or hex values appear mid-file.
