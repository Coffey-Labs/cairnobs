#!/usr/bin/env python3
"""Convert the <text> in the brand lockups to <path> outlines.

Why: the lockups in src/lib/assets are loaded via <img src> (and inlined as
data URIs at build time). An <img>-loaded SVG is an isolated document -- it
cannot reach the page's @font-face rules, so the app's self-hosted Overpass
Mono never applies to a <text> element inside one. The wordmark therefore
rendered in whatever each visitor's default monospace happened to be, and
changed shape by platform. Outlining against the font we already ship makes
it pixel-stable everywhere and matches the product's own typography.

Re-run this after refreshing the logo package from upstream (upstream ships
live <text>). Idempotent: files with no <text> left are skipped.

    python3 web/scripts/outline-wordmark.py

Only <text> is rewritten; every other byte of each file is left alone.
"""

import re
import sys
from pathlib import Path
from xml.dom import minidom

from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont
from fontTools.varLib import instancer

ROOT = Path(__file__).resolve().parent.parent
FONT = ROOT / "static/fonts/overpass-mono-variable.woff2"
ASSETS = ROOT / "src/lib/assets"

# Every lockup sets font-weight 700; the variable font defaults to 300.
WEIGHT = 700

TEXT_RE = re.compile(r"[ \t]*<text\b.*?</text>\s*?\n", re.DOTALL)


def ntos(v):
    """Two decimals is ~0.005px at the largest size any lockup is drawn at.

    The default str() emits full float repr (15.264000000000001), which more
    than doubles the path data for no visible gain -- and these files ship
    inlined in the bundle.
    """
    return f"{v:.2f}".rstrip("0").rstrip(".") or "0"


def load_glyphset():
    font = TTFont(FONT)
    font = instancer.instantiateVariableFont(font, {"wght": WEIGHT})
    return font.getGlyphSet(), font.getBestCmap(), font["hmtx"], font["head"].unitsPerEm


def runs_of(node):
    """Flatten a <text> into (string, fill-override) runs, in document order.

    Handles the one nesting the lockups use: bare text plus <tspan fill=...>.
    """
    out = []
    for child in node.childNodes:
        if child.nodeType == child.TEXT_NODE:
            if child.data:
                out.append((child.data, None))
        elif child.nodeType == child.ELEMENT_NODE and child.localName == "tspan":
            text = "".join(c.data for c in child.childNodes if c.nodeType == c.TEXT_NODE)
            out.append((text, child.getAttribute("fill") or None))
    return [(t, f) for t, f in out if t]


def outline(gs, cmap, hmtx, upem, runs, x, y, size, spacing, anchor):
    """Return [(fill, path-d)], laying runs out left to right from the anchor."""
    scale = size / upem
    advances = [hmtx[cmap[ord(c)]][0] * scale for t, _ in runs for c in t]
    n = len(advances)
    # Trailing letter-spacing is excluded: it is not ink, and including it
    # would bias a centred lockup half a unit to the left.
    width = sum(advances) + spacing * (n - 1) if n else 0.0

    if anchor == "middle":
        cursor = x - width / 2
    elif anchor == "end":
        cursor = x - width
    else:
        cursor = x

    paths = []
    for text, fill in runs:
        pen = SVGPathPen(gs, ntos=ntos)
        for ch in text:
            gname = cmap[ord(ch)]
            # y flips: font units are y-up, SVG user space is y-down.
            gs[gname].draw(TransformPen(pen, (scale, 0, 0, -scale, cursor, y)))
            cursor += hmtx[gname][0] * scale + spacing
        paths.append((fill, pen.getCommands()))
    return paths


def convert(path, gs, cmap, hmtx, upem):
    src = path.read_text()
    blocks = TEXT_RE.findall(src)
    if not blocks:
        return False

    out = src
    for block in blocks:
        node = minidom.parseString(block.strip()).documentElement
        indent = re.match(r"[ \t]*", block).group(0)

        runs = runs_of(node)
        base_fill = node.getAttribute("fill") or "#000"
        paths = outline(
            gs, cmap, hmtx, upem, runs,
            x=float(node.getAttribute("x") or 0),
            y=float(node.getAttribute("y") or 0),
            size=float(node.getAttribute("font-size")),
            spacing=float(node.getAttribute("letter-spacing") or 0),
            anchor=node.getAttribute("text-anchor") or "start",
        )

        rendered = "".join(
            f'{indent}<path fill="{fill or base_fill}" d="{d}"/>\n'
            for fill, d in paths if d
        )
        out = out.replace(block, rendered, 1)

    path.write_text(out)
    return True


def main():
    if not FONT.exists():
        sys.exit(f"font not found: {FONT}")
    gs, cmap, hmtx, upem = load_glyphset()

    touched = 0
    for svg in sorted(ASSETS.glob("*.svg")):
        if convert(svg, gs, cmap, hmtx, upem):
            print(f"outlined  {svg.relative_to(ROOT)}")
            touched += 1
        else:
            print(f"no <text> {svg.relative_to(ROOT)}")
    print(f"\n{touched} file(s) rewritten")


if __name__ == "__main__":
    main()
