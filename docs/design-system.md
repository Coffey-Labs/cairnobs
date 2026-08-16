# Sentry Design System (Phase 5)

Direction: **Signal** — cockpit/ICU-monitor instrumentation logic. Color
is a rationed resource: the UI is near-neutral grayscale everywhere, so
the four severity tiers land as genuinely the most saturated colors
anywhere on screen, not one saturated color competing with a dozen
decorative ones. See the Phase 5 design-direction review (three options
were presented; Signal was the one picked) for the two rejected
directions and the full rationale.

This doc is the source of truth for the token system and component
library `/web/src/lib/components/ui` implements — read it before adding
a new color, spacing value, or component, rather than reaching for a
literal hex or one-off markup the way Phase 0-3's pages did.

## Status

**Status: Phase 5 shipped.** All ten tasks are built: the token system,
the `ui/` component library, the persistent sidebar nav + command
palette, a real ECharts-based charting layer (5 chart types, drill-down,
zoom), the dashboard panel rebuild (GridStack drag-and-drop + live-preview
panel editor), the query/search redesign (CodeMirror syntax highlighting
+ autocomplete, sortable/resizable/expandable results), the alerting UI
redesign (severity-state pill + delivery timeline), and an accessibility
pass driven by real axe-core runs against the live app (not static
analysis) — see the Accessibility section below for what that actually
caught. See `/docs/phase-5-runbook.md` for the full verification log.

Verified in a real browser against a live docker-compose stack with real
seeded data (not just fixtures): dark/light/system theme and
comfortable/compact density switching persisting across navigation and
reload; command palette open/filter/keyboard-navigate/go-to; every nav
route including dashboard/alert detail pages with real panels and real
alert history; all 6 panel viz types rendering real query results; a
firing alert rule's full state-history timeline; keyboard-only operation
of the query editor, results table (sort/expand), and dashboard grid.

## Fonts

Self-hosted (not a Google Fonts CDN link — no runtime dependency on a
third party for the app to render correctly): `web/static/fonts/`,
licensed under the SIL Open Font License (see that directory's
`LICENSE.txt`).

| Role | Typeface | Why |
|---|---|---|
| UI (nav, labels, headings, body) | Overpass | Originally drawn for U.S. highway signage — engineered to be read correctly, fast, under bad conditions. A more honest reason to pick a typeface for an incident-response tool than "it looks modern." |
| Data (log tables, code, query bar, numbers) | Overpass Mono | Same family as the UI face — one typeface end to end removes even the small cognitive cost of a font pairing, matching the direction's overall restraint. |

Both are variable fonts (one file covers the full weight range), loaded
via `@font-face` in `web/src/lib/styles/fonts.css`.

## Color tokens

Defined in `web/src/lib/styles/tokens.css`. Dark is the literal default
— `:root` defines the dark palette directly, light is the override
(both `@media (prefers-color-scheme: light)` for an unset preference and
`[data-theme="light"]` for an explicit one) — not a retrofit where light
is `:root` and dark is bolted on. Component CSS must only ever read a
token, never a literal hex; that's what makes the theme/density toggles
a token swap instead of a per-component rewrite.

| Token | Dark | Light | Use |
|---|---|---|---|
| `--color-bg` | `#0a0a0b` | `#f7f7f8` | Page background |
| `--color-surface` | `#17181a` | `#ffffff` | Cards, tables, inputs |
| `--color-surface-raised` | `#1e2023` | `#ffffff` | Hover states, popovers |
| `--color-border` / `--color-border-strong` | `#2a2c2f` / `#3a3d41` | `#dfe0e2` / `#c7c9cc` | Dividers, input borders |
| `--color-text` / `--color-text-muted` / `--color-text-faint` | `#f0f0f1` / `#85888d` / `#8a8d92` | `#101113` / `#6b6e73` / `#75787d` | Body text hierarchy |
| `--color-accent` / `--color-accent-strong` | `#3fb6ff` | `#0b84d6` | Interactive elements only — links, primary buttons, focus rings, active nav. Never reused for severity (see below); a semantic color competing with the brand accent defeats the point of "color means something." |

### Severity tiers

The schema carries seven OTel severities
(`TRACE`/`DEBUG`/`INFO`/`WARN`/`ERROR`/`FATAL`/`UNSPECIFIED`, see
`/storage/README.md`). Seven colors would be seven things to memorize at
a glance; `web/src/lib/severity.ts`'s `severityTier()` collapses them to
four accent tiers plus one "quiet" state:

| OTel severity | Tier | Dark | Light |
|---|---|---|---|
| `TRACE`, `DEBUG`, `UNSPECIFIED` | quiet | `#85888d` | `#6b6e73` |
| `INFO` | info | `#4c8dff` | `#1a63d6` |
| `WARN` | warn | `#f5c242` | `#8c6800` |
| `ERROR` | error | `#ff6a39` | `#c94b1e` |
| `FATAL` | critical | `#ff2d78` | `#c21362` |

Chosen as a blue → amber → orange → magenta progression — hue *and*
lightness both shift at every step, so no two adjacent tiers rely on
red-vs-green to be told apart, and the sequence should survive grayscale
and protanopia/deuteranopia simulation. That reasoning hasn't been
verified with an actual simulator yet — do that before treating it as
confirmed accessible, not just plausible.

Each tier also has a translucent `-bg` token (e.g. `--color-sev-warn-bg`)
for chip/pill backgrounds. These are tuned independently of the solid
foreground colors above, not derived from them by a fixed formula: a
translucent color tints *toward* its own hue as alpha increases, which
for these saturated, low-luminance hues (critical's magenta especially)
*lowers* contrast against the foreground text the higher the alpha goes
— the opposite of the intuitive "more opaque background is safer"
assumption. `--color-sev-warn` itself was darkened in light mode
(`#9c7300` → `#8c6800`) for the same underlying reason: axe-core caught
the *plain* light-mode warn text failing AA (4.32:1) against white
before any background was even involved. See the Accessibility section.

Use `<SeverityBadge severity={row.severity} />`
(`ui/SeverityBadge.svelte`) to render one of these — it owns the OTel
string → tier mapping so call sites can't invent a sixth color by hand.
For non-log-severity status (success/danger/neutral/accent — e.g. a
"saved" confirmation), use `<Badge tone="...">` instead; the two are
deliberately separate components so a form-validation color can never
accidentally collide with a log-severity one.

## Type scale

14px base (dense-first — this is a tool for reading log tables, not
marketing copy), 1.2 modular ratio:

`--text-xs` 11px · `--text-sm` 13px · `--text-base` 14px · `--text-md`
16px · `--text-lg` 20px · `--text-xl` 28px · `--text-2xl` 40px

`--font-weight-normal` 400, `--font-weight-medium` 600,
`--font-weight-bold` 700. `font-variant-numeric: tabular-nums` is set
globally on `body` so columns of numbers/timestamps align.

## Spacing, radius, shadow

8px-based spacing scale: `--space-1` through `--space-8` (4px, 8px,
12px, 16px, 24px, 32px, 48px, 64px). Radius: `--radius-sm` 4px,
`--radius-md` 6px, `--radius-lg` 10px, `--radius-full` (pills). Shadow
is used sparingly, matching Signal's restraint — only real overlays
(modals, the command palette, tooltips) get one (`--shadow-sm/md/lg`);
inline UI never does.

## Density

Two presets, one token swap — `html.density-compact` overrides
`--row-height`, `--row-padding-y/x`, `--panel-padding`, `--control-height`,
and drops `--text-base` to `--text-sm`. Comfortable (the default) suits
dashboards and forms; compact suits log tables and query results. Toggle
via `$lib/density.svelte.ts`'s `setDensity()`/`toggleDensity()` — a
global, persisted (`localStorage['sentry.density']`) preference, not a
per-page setting, so switching it on one page carries to the next.
`web/src/app.html` has a synchronous inline script that applies the
stored value before first paint, so there's no flash of the wrong
density on reload; keep that script's storage key/values in sync with
`density.svelte.ts` if either changes.

## Theme

`$lib/theme.svelte.ts`, same persisted/synchronous-apply shape as
density. Three states — `'dark' | 'light' | 'system'` — but unlike most
apps, the *unset* default is `'dark'`, not `'system'`. That's the actual
point of "real dark mode as the default, not an afterthought": a
first-time visitor on a light-OS machine still lands in dark. `'system'`
is available as a deliberate opt-in for anyone who wants their OS
setting to win instead.

## Components (`web/src/lib/components/ui/`)

Import from the barrel: `import { Button, Input, ... } from
'$lib/components/ui';`

| Component | Notes |
|---|---|
| `Button` | `variant`: `primary`/`secondary`/`ghost`/`danger`. `size`: `sm`/`md`. Renders `<a>` when `href` is passed, `<button>` otherwise. |
| `Input` | Thin styled wrapper over `<input>`; `bind:value`, `invalid` sets `aria-invalid` + a danger border. |
| `Select` | Styled wrapper over native `<select>` (real `<option>` children) — not a custom listbox, so it keeps native keyboard/screen-reader behavior for free. |
| `Badge` | Generic status pill. `tone`: `neutral`/`success`/`danger`/`accent`. |
| `SeverityBadge` | `severity` prop takes a raw OTel string; maps to a tier internally. Use this instead of `Badge` for anything log-severity-shaped. |
| `Table` | A styling wrapper around real `<table>` markup (native semantics matter for screen readers) — pass real `<thead>`/`<tbody>` as children. Density-aware via the row tokens above. Not a data grid; sort/resize (Phase 5 task 6) layers on top later. |
| `Card` | `title`/`actions` (a snippet) header, `padded` toggle. |
| `Modal` | Built on native `<dialog>` — real focus trap, Escape-to-close, and top-layer stacking, not hand-rolled. `bind:open`, `title`, `footer` snippet. |
| `Tooltip` | Pure-CSS hover/focus tooltip, `role="tooltip"` + `aria-describedby`. |
| `Tabs` | Renders the tab list only (roving tabindex, arrow-key nav); the caller renders each panel's content on `bind:active` and owns `id="panel-{id}"`/`aria-labelledby="tab-{id}"`. |

`CommandPalette.svelte` and `NavSidebar.svelte`
(`web/src/lib/components/`, not under `ui/`) are app-shell components,
not general-purpose library pieces — mounted once in `+layout.svelte`.

## Navigation & tenant switching

Persistent sidebar (`NavSidebar.svelte`): Search / Dashboards / Alerts /
Data Sources / Settings, active-route highlighting, a command-palette
hint, and the theme/density quick toggles. The tenant indicator calls
`getCurrentSession()` (`$lib/api.ts`), which wraps `POST
/internal/authorize` — an endpoint that already existed (`api/authz.
HTTPAuthorizer` and `alerting` already call it) and was already reachable
from the browser (`enterprise-auth`'s whole mux is behind
`WithCredentialedCORS`, not just the tenant-picker routes), so this
needed zero backend changes.

**Known limitation, not yet solved at the API level**: there is no
"list my other tenant memberships while already logged in" endpoint —
`GET /auth/memberships` only works during the short-lived pending-login
window (see `enterprise/internal/loginhandler`'s doc comment), not for
an established session. So "Switch tenant" in the sidebar re-triggers
login (`/auth/oidc/login`) rather than offering an inline dropdown —
logging in again is the only tenant-switching mechanism this product
actually has today, and if there's more than one membership it
naturally lands back on `/select-tenant`. A real inline switcher would
need a new `GET /auth/my-memberships`-shaped endpoint (or equivalent);
that's real, disclosed future work, not a bug in what's built.

## Command palette

`⌘K`/`Ctrl+K` anywhere in the app. Indexes the five static nav
destinations plus live-fetched dashboards and alert rules
(`listDashboards()`/`listRules()`, via `Promise.allSettled` so one
failing endpoint doesn't blank the other's results or the static items).
Arrow keys + Enter to navigate, Escape to close (native `<dialog>`,
same reasoning as `Modal`). Substring filter, not fuzzy — no new
dependency pulled in for it, matching this repo's "boring,
well-understood dependencies" convention.

## Charting (`web/src/lib/charts/`)

Built on **ECharts**, via modular imports (`echarts/core` plus only the
chart/component modules actually used — `LineChart`, `BarChart`,
`HeatmapChart`, `TooltipComponent`, `GridComponent`, `LegendComponent`,
`DataZoomComponent`, `VisualMapComponent`, `MarkLineComponent`,
`CanvasRenderer`), not the full bundle. Picked over Observable Plot (SVG
rendering hits a real performance ceiling at volume, and no built-in
zoom/pan or legend-toggle) and raw D3 (too much hand-rolled engineering
for chart types this standard). Verified, not assumed: the lazy-loaded
chart chunk is 211,975 bytes gzipped, and a synthetic 30,006-row/6-series
stress fixture (`/dev/charts`, an unlisted dev-only route) renders its
first two frames in ~50ms on a production build — `npm run dev`'s
~3.1s figure for the same fixture is pure dev-mode/unminified-JS
overhead, not a real perf number, and was confirmed as such before being
discarded.

Five chart components, all consuming the same `{columns, rows}`
`QueryResult` shape Phase 2's query endpoint has always returned:
`TimeSeriesChart` (multi-series line, legend toggle), `BarChart`
(including stacked), `SingleStat` (big number + sparkline + trend),
`Heatmap`, `TopN` (ranked horizontal bars). `EChart.svelte` is the shared
base wrapper (init/resize/dispose lifecycle).

**No query-language change was needed for multi-series or drill-down** —
both are pure frontend reshaping of the existing tabular output:

- `pivot.ts`'s `pivot(columns, rows, config)` turns a "long" result (one
  row per series+x pair, e.g. `stats count by service, timestamp`) into
  one series per distinct value of the grouping column. Its value-column
  auto-detection has one sharp edge worth knowing if you touch it: when
  a result has only one non-x column (a bare `stats count`, `single_stat`'s
  most common shape), `Array.prototype.findIndex` returning `-1` for "not
  found" must be checked explicitly — `-1 ?? fallback` never falls back,
  because `-1` isn't `null`/`undefined`. This was a real, shipped bug
  (every `single_stat` panel silently rendered `0`) caught by seeding a
  live dashboard with real query results rather than only fixture data,
  not by any static check.
- `drilldown.ts` strips a panel's query down to its pre-`stats` filter
  portion, appends a clicked series/x-value as a new filter term, and
  computes a tight time window around a clicked timestamp — then
  navigates to the Search page via URL params (`?q=&earliest=&latest=`).

`theme.ts`'s `readChartTokens()` reads real computed CSS custom-property
values (`getComputedStyle`) so charts render in the actual active
theme's colors rather than a hardcoded palette — guarded with an
`SSR_FALLBACK` object for adapter-static's prerender pass, where
`document` doesn't exist.

**The one backend change in this phase**: `heatmap` as a `VizType`
(`api/dashboards/types.go`'s `validVizType()`, `web/src/lib/api.ts`'s
`VizType` union, and the `dashboard_panels` table's `viz_type` CHECK
constraint — three places, not two; the DB constraint mirrors the Go
validator and was originally missed, which meant a heatmap panel passed
API validation and then failed on insert. See
`metadata/migrations/0035_add_heatmap_viz_type.sql`). Justified under
the brief's "no query-language or data-model changes except what's
strictly needed to feed richer visualizations" exception — a heatmap is
a viz type, not a new query capability.

## Dashboard panels

Drag-and-drop grid is **GridStack**, already a Phase 3 dependency — no
new library needed, satisfying the brief's "a maintained library is
fine, don't hand-roll grid physics." `PanelEditor.svelte` (a `Modal`)
replaces the old inline add-panel form: a debounced live preview reuses
`PanelViz` directly, so what you see while editing is pixel-identical to
what renders on save, not a separate preview renderer that can drift
from the real thing. Empty/loading/error states use `EmptyState` and
`Skeleton` rather than a blank panel or a raw error string.

## Query & search (`web/src/lib/query-editor/`)

**CodeMirror 6**, not a hand-rolled textarea-plus-overlay highlighter —
picked specifically because autocomplete needs real cursor-aware popup
positioning, which a plain textarea can't give you. `language.ts` is a
`StreamLanguage` tokenizer for the pipe grammar; its `token()` function
must return real `@lezer/highlight` tag names looked up by string
(`'controlKeyword'`, `'operatorKeyword'`, `'name.function'` for
tag+modifier pairs) — a custom `Tag.define()` object's `.toString()`
looks plausible but silently fails to highlight anything, a real bug hit
and fixed while building this. `completions.ts` provides context-aware
suggestions: stage keywords after `|`, stats functions after `stats`,
field names elsewhere.

`ResultsTable.svelte`: sortable columns (click header — a real `<button>`
inside the `<th>`, not a clickable `<th>` itself, so it's keyboard-operable
for free), resizable columns (pointer-drag handles, mouse-only by
design — the `role="separator"` handle is intentionally not in the tab
order, matching how most apps treat column resize as a mouse affordance
rather than a keyboard one), and expandable rows. The row-expand
interaction was originally a bare `<tr onclick=...>` with no keyboard
equivalent at all — a real gap the accessibility pass caught (not via
axe, which doesn't flag missing keyboard handlers on custom widgets;
caught by manually tabbing through the page), fixed by adding
`tabindex="0"`, `role="button"`, `aria-expanded`, and an `Enter`/`Space`
`onkeydown` handler alongside the existing click handler.

"Add as panel to dashboard" (`AddToDashboardModal.svelte`) lets a query
built on the Search page become a saved panel without hand-copying the
query string into the dashboard editor.

## Alerting UI

`AlertStatePill.svelte` reuses the log-severity color tiers rather than
inventing a second color vocabulary: `ok → quiet`, `pending → warn`,
`firing → critical`. `DeliveryTimeline.svelte` reframes the existing
`delivery_log` data (no new backend fields) as a vertical timeline —
"why didn't I get paged" is a chronological question, and a flat table
answered it less directly than a timeline does.

## Accessibility

Automated checks ran with **axe-core**, injected into the live app in a
real browser and driven against a real seeded docker-compose stack
(dashboards with all 6 panel viz types populated with real query
results, a firing alert rule with real delivery history) — not just
static markup or empty/loading states, since several of the real
findings below only appear once real content renders. Real violations
found and fixed:

- `color-contrast`: `AlertStatePill`'s critical tier at 4.04:1 (below
  the 4.5:1 AA threshold) when rendered on an opaque `--color-surface`
  card, not the plain page background most severity chips happen to sit
  on — see the Severity tiers section above for why translucent
  backgrounds made this worse, not better, and why quiet/info (dark) and
  quiet/warn/error (light) were fixed alongside it as the same latent
  bug, not yet triggered elsewhere only because no page had rendered
  those specific tiers on an opaque surface yet.
- `landmark-main-is-top-level` / `landmark-no-duplicate-main` /
  `landmark-unique`: the layout shell's content wrapper was a second
  `<main>` nested around each page's own top-level `<main>`.
- `landmark-one-main` / `region`: `/data-sources` was the one page using
  a plain `<div>` instead of `<main>` as its top-level element.
- `heading-order`: `Card` and `EmptyState` both jumped from `<h1>` to
  `<h3>`, skipping `<h2>`.
- `aria-input-field-name`: the CodeMirror query editor's `.cm-content`
  had no accessible name — fixed via `EditorView.contentAttributes.of({
  'aria-label': ... })`.
- `empty-table-header`: the alerts list table's trailing actions column
  had a bare `<th></th>` — fixed with visually-hidden text (`.sr-only`,
  now a shared utility in `app.css`) rather than a visible "Actions"
  label that would've added a fifth, less-necessary heading.

Keyboard-only operation was verified by tabbing through, not just
inferred from markup: sidebar nav → command-palette hint → theme/density
controls → into main content, with a visible focus ring
(`--focus-ring`, a two-layer box-shadow so it's visible on both light
and dark surfaces) at every stop; the query editor is reachable and
labeled; sort buttons are real `<button>`s; the results table's row
expansion works via `Enter`/`Space` after the fix above. Responsive:
the sidebar collapses to an off-canvas drawer under 860px
(`NavSidebar.svelte`'s `mobileOpen`/`onCloseMobile`, a transform-based
slide-in with a backdrop button), verified down to tablet-landscape
width, not just described in CSS.
