# Brand assets

Cairn OBS logo package **v2 ("faceted glow")**. Source package:
`cairn-obs-logo-v2`, copied in here and renamed to this directory's
convention (the `cairn-obs-` prefix is dropped -- the directory already
says whose brand it is).

Everything is a plain `<polygon>` over four shared gradient stops, so a
re-theme is a find/replace on the stops rather than a redraw.

## What the app actually uses

| File | Used by |
| --- | --- |
| `favicon.svg` | `routes/+layout.svelte` (`<link rel="icon">`) |
| `logo-horizontal-dark.svg` / `-light.svg` | `components/NavSidebar.svelte`, picked on theme |
| `logo-stacked-dark.svg` / `-light.svg` | `routes/+page.svelte` (landing), picked on theme |

The rest are carried for completeness and are **not** imported anywhere,
so Vite does not emit them into the bundle:

- `icon-glow.svg` / `icon-flat.svg` -- mark only, with and without the
  ambient glow. Use `-flat` anywhere an SVG filter gets stripped.
- `icon-mono-white.svg` / `icon-mono-black.svg` -- single-colour
  silhouettes.
- `wordmark-dark.svg` / `wordmark-light.svg` -- text only.
- `hero-grid.svg` -- 800x480 banner scene (social card, splash).

Raster favicons live in `web/static/icons/` (16/32/48/180/512), served
from `/icons/` and referenced by absolute path in the layout head.

## Two things to know before editing

**`logo-stacked-light.svg` is derived, not vendored.** The v2 package
ships no light-background stacked lockup, but the landing page needs one
for light theme. It is the stacked-dark file with the treatment the
package itself applies to its horizontal pair: same stone geometry and
gradients, ambient glow dropped (it reads as haze on white), wordmark
switched to the light-surface ink and accent (`#111315` / `#E6690E`).
If the package is refreshed, re-derive it rather than assuming an
upstream file appeared.

**The wordmark is outlined, not live text.** Upstream ships the lockups
with a `<text>` element set in `'JetBrains Mono','Fira Code',ui-monospace,
monospace`. That does not survive the way this app loads them: they go
through `<img src>` (and get inlined as data URIs at build time), and an
`<img>`-loaded SVG is an isolated document that cannot reach the page's
`@font-face` rules. Neither our self-hosted Overpass Mono nor JetBrains
Mono applied, so the wordmark fell back to each visitor's default
monospace and changed shape by platform.

Every `<text>` has therefore been converted to `<path>` outlines set in
**Overpass Mono Bold** -- the font the app already ships and uses for its
own monospace UI, so the lockup and the interface now agree. The
conversion is scripted:

    python3 web/scripts/outline-wordmark.py

Re-run it after refreshing the package from upstream (upstream will ship
live `<text>` again). It rewrites only `<text>` elements, leaves every
other byte alone, and skips files that have none, so it is safe to re-run.
Editing the wordmark's copy or spacing now means editing the upstream
`<text>` and re-running the script -- not hand-editing path data.

## Repository social preview

**`/docs/assets/social-preview.png` is derived, not vendored.** GitHub's
social card wants a 1280x640 raster; the package ships no such file, and
`hero-grid.svg` alone shows the mark with no project name -- fine as a
splash, wrong for a card that several platforms render without any
accompanying repo title.

Built from two files already here, so it re-derives rather than needing
a redraw:

```sh
# 1. hero-grid without its cairn: the four stones live in one
#    <g transform="translate(200,60)"> -- drop that group and the base
#    fill, grid pattern and vignette are left intact.
# 2. render background at target resolution, then crop 5:3 -> 2:1.
#    96px off the top (empty grid) and 32px off the bottom, NOT a
#    centre-crop: the composition sits low and crowds the bottom edge.
rsvg-convert -w 1280 -h 768 hero-grid-no-cairn.svg -o bg.png
magick bg.png -crop 1280x640+0+96 +repage bg640.png

# 3. composite the package's own horizontal lockup, so mark-to-wordmark
#    spacing stays the designer's rather than something re-invented.
rsvg-convert -w 900 -h 225 logo-horizontal-dark.svg -o lockup.png
magick bg640.png lockup.png -gravity center -composite social-preview.png
```

`-dark` on purpose: the banner ground is `#0A0B10`, so the lockup needs
the `#F2F2F2` ink meant for dark surfaces.

Re-running the above reproduces the committed file pixel for pixel
(`magick compare -metric AE` returns 0), but *not* byte for byte -- PNG
writes a timestamp chunk. Compare pixels, not `sha256sum`.

The wide gaps in `cairn   obs   |` are the wordmark's intended spacing,
not a missing font -- the glyphs are outlined `<path>` data and there is
no `font-family` anywhere in the file. Do not "fix" them.

**Not yet applied.** GitHub only offers Settings -> General -> Social
preview on public repositories; the section is absent while this repo is
private. Upload it when that changes.

## Aspect ratios

Changed from v1 -- constrain one axis and leave the other `auto`:

- horizontal lockups: 640x160 (v1 was 600x140)
- stacked lockups: 360x320, **not square** (v1 was 360x360)
