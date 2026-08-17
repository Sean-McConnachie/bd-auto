#!/usr/bin/env python3
"""Render a captured terminal screen to a PNG.

`tmux capture-pane -p -e` gives back the pane exactly as it stood, escape
codes and all. That is the honest record of what the table looked like, but it
is not something anyone can put in a document, so this turns one into an image:
a fixed-pitch grid, the 256-colour palette, bold and reverse, and a window
frame around it so a screenshot reads as a screenshot.

    ansi2png.py capture.ansi out.png --title "wave 2, three workers running"

It depends on Pillow and a monospace font on the system, and on nothing else.
"""

from __future__ import annotations

import argparse
import re
import sys
from dataclasses import dataclass, replace

from PIL import Image, ImageDraw, ImageFont

# The window frame. Dark, because a terminal is.
BG = (13, 17, 23)
CHROME = (28, 33, 40)
CHROME_TEXT = (139, 148, 158)
FG = (201, 209, 217)
LIGHTS = ((255, 95, 86), (255, 189, 46), (39, 201, 63))

FONTS = [
    "/usr/share/fonts/adwaita-mono-fonts/AdwaitaMono-Regular.ttf",
    "/usr/share/fonts/liberation-mono-fonts/LiberationMono-Regular.ttf",
    "/usr/share/fonts/google-noto-vf/NotoSansMono[wght].ttf",
]
BOLD_FONTS = [
    "/usr/share/fonts/adwaita-mono-fonts/AdwaitaMono-Bold.ttf",
    "/usr/share/fonts/liberation-mono-fonts/LiberationMono-Bold.ttf",
    "/usr/share/fonts/google-noto-vf/NotoSansMono[wght].ttf",
]

CSI = re.compile(r"\x1b\[([0-9;:]*)([A-Za-z])")
OSC = re.compile(r"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)")


def palette() -> list[tuple[int, int, int]]:
    """The xterm 256-colour table."""
    base = [
        (0, 0, 0), (205, 49, 49), (13, 188, 121), (229, 229, 16),
        (36, 114, 200), (188, 63, 188), (17, 168, 205), (229, 229, 229),
        (102, 102, 102), (241, 76, 76), (35, 209, 139), (245, 245, 67),
        (59, 142, 234), (214, 112, 214), (41, 184, 219), (255, 255, 255),
    ]
    out = list(base)
    steps = (0, 95, 135, 175, 215, 255)
    for r in steps:
        for g in steps:
            for b in steps:
                out.append((r, g, b))
    for i in range(24):
        v = 8 + i * 10
        out.append((v, v, v))
    return out


PALETTE = palette()


@dataclass(frozen=True)
class Cell:
    ch: str = " "
    fg: tuple[int, int, int] | None = None
    bg: tuple[int, int, int] | None = None
    bold: bool = False
    underline: bool = False


@dataclass(frozen=True)
class Pen:
    fg: tuple[int, int, int] | None = None
    bg: tuple[int, int, int] | None = None
    bold: bool = False
    underline: bool = False
    reverse: bool = False


def sgr(pen: Pen, params: str) -> Pen:
    """Fold one SGR sequence into the pen."""
    # Sub-parameters are colon separated in the newer form; both spellings mean
    # the same thing here, so flatten them.
    codes = [int(p) for p in re.split("[;:]", params) if p != ""] or [0]
    i = 0
    while i < len(codes):
        c = codes[i]
        if c == 0:
            pen = Pen()
        elif c == 1:
            pen = replace(pen, bold=True)
        elif c == 2:
            pen = replace(pen, bold=False)
        elif c == 4:
            pen = replace(pen, underline=True)
        elif c == 7:
            pen = replace(pen, reverse=True)
        elif c in (21, 22):
            pen = replace(pen, bold=False)
        elif c == 24:
            pen = replace(pen, underline=False)
        elif c == 27:
            pen = replace(pen, reverse=False)
        elif 30 <= c <= 37:
            pen = replace(pen, fg=PALETTE[c - 30])
        elif 90 <= c <= 97:
            pen = replace(pen, fg=PALETTE[c - 90 + 8])
        elif 40 <= c <= 47:
            pen = replace(pen, bg=PALETTE[c - 40])
        elif 100 <= c <= 107:
            pen = replace(pen, bg=PALETTE[c - 100 + 8])
        elif c == 39:
            pen = replace(pen, fg=None)
        elif c == 49:
            pen = replace(pen, bg=None)
        elif c in (38, 48):
            colour, i = extended(codes, i)
            pen = replace(pen, fg=colour) if c == 38 else replace(pen, bg=colour)
        i += 1
    return pen


def extended(codes: list[int], i: int) -> tuple[tuple[int, int, int] | None, int]:
    """Read a 38/48 extended colour, returning it and the index it ended on."""
    if i + 1 < len(codes) and codes[i + 1] == 5 and i + 2 < len(codes):
        return PALETTE[codes[i + 2] % 256], i + 2
    if i + 1 < len(codes) and codes[i + 1] == 2 and i + 4 < len(codes):
        return (codes[i + 2], codes[i + 3], codes[i + 4]), i + 4
    return None, i


def screen(text: str) -> list[list[Cell]]:
    """Turn a captured pane into a grid of cells."""
    text = OSC.sub("", text.replace("\r\n", "\n"))
    rows: list[list[Cell]] = []
    for line in text.split("\n"):
        pen, row, pos = Pen(), [], 0
        while pos < len(line):
            m = CSI.match(line, pos)
            if m:
                if m.group(2) == "m":
                    pen = sgr(pen, m.group(1))
                pos = m.end()
                continue
            ch = line[pos]
            pos += 1
            if ch == "\x1b" or ch == "\x0f":
                continue
            fg, bg = pen.fg, pen.bg
            if pen.reverse:
                fg, bg = bg or BG, fg or FG
            row.append(Cell(ch, fg, bg, pen.bold, pen.underline))
        rows.append(row)
    while rows and not "".join(c.ch for c in rows[-1]).strip():
        rows.pop()
    return rows


def render(rows: list[list[Cell]], out: str, title: str, size: int, pad: int) -> None:
    font = load(FONTS, size)
    bold = load(BOLD_FONTS, size)
    probe = font.getbbox("M")
    cw = max(1, probe[2] - probe[0])
    # A cell is one advance wide; measure it rather than the ink of one glyph,
    # or every column drifts left.
    cw = round(font.getlength("M" * 100) / 100)
    ascent, descent = font.getmetrics()
    ch = ascent + descent + 2

    cols = max((len(r) for r in rows), default=1)
    cols = max(cols, len(title) + 4)
    bar = ch + pad if title else 0
    w = cols * cw + pad * 2
    h = len(rows) * ch + pad * 2 + bar

    img = Image.new("RGB", (w, h), BG)
    d = ImageDraw.Draw(img)

    if title:
        d.rectangle([0, 0, w, bar], fill=CHROME)
        for i, colour in enumerate(LIGHTS):
            x = pad + i * (ch // 2)
            d.ellipse([x, bar // 2 - 4, x + 8, bar // 2 + 4], fill=colour)
        d.text((pad + 3 * (ch // 2) + 10, bar // 2), title, font=font,
               fill=CHROME_TEXT, anchor="lm")

    for y, row in enumerate(rows):
        top = pad + bar + y * ch
        for x, cell in enumerate(row):
            left = pad + x * cw
            if cell.bg:
                d.rectangle([left, top, left + cw, top + ch], fill=cell.bg)
            if cell.ch != " ":
                d.text((left, top + 1), cell.ch, font=bold if cell.bold else font,
                       fill=cell.fg or FG)
            if cell.underline:
                d.line([left, top + ascent + 1, left + cw, top + ascent + 1],
                       fill=cell.fg or FG)
    img.save(out)


def load(paths: list[str], size: int) -> ImageFont.FreeTypeFont:
    for p in paths:
        try:
            return ImageFont.truetype(p, size)
        except OSError:
            continue
    print("no monospace font found; tried:\n  " + "\n  ".join(paths), file=sys.stderr)
    raise SystemExit(1)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("input")
    ap.add_argument("output")
    ap.add_argument("--title", default="")
    ap.add_argument("--size", type=int, default=15)
    ap.add_argument("--pad", type=int, default=14)
    a = ap.parse_args()
    with open(a.input, encoding="utf-8", errors="replace") as f:
        render(screen(f.read()), a.output, a.title, a.size, a.pad)


if __name__ == "__main__":
    main()
