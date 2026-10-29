#!/usr/bin/env python3
"""RubbleSAR offline incident map (phase 6): draws street-level map tiles of
the incident area from rescue-dispatch's layers file, for WinTAK and ATAK.

  render_map.py --layers FILE --out DIR [--mbtiles FILE] [--min-zoom 13]
                [--max-zoom 19] [--jobs N] [--font FILE] [--bold-font FILE]
                [--legend FILE] [--overview FILE]

Writes DIR/{z}/{x}/{y}.png (the XYZ layout a "{$z}/{$x}/{$y}" map source
reads, served by sar-web) and, with --mbtiles, the same tiles as an MBTiles
file to import into WinTAK or ATAK (no server needed).

What it draws: land, green areas, water and rivers, buildings, streets by
class with their names, streets closed by rubble (red) or in OpenStreetMap
(red dashes), the simulated collapsed buildings and their rubble, the
command post. Outside the downloaded area the map fades.

Offline tooling (Python 3 and Pillow: apt install python3-pil
fonts-dejavu-core). Map data (c) OpenStreetMap contributors, ODbL.
"""

import argparse
import json
import math
import os
import sqlite3
import sys
import time
from multiprocessing import Pool

from PIL import Image, ImageDraw, ImageFont

# Pillow 9.1 moved the resampling filters to Image.Resampling; Ubuntu 22.04
# (JetPack 6 on the Jetson) ships Pillow 9.0, which has them on Image.
RESAMPLE = getattr(Image, "Resampling", Image)

TILE = 256
SS = 2  # supersampling for anti-aliased lines
MARGIN = 48  # pixels drawn around a tile so lines and labels cross its edges cleanly

FONT_PATHS = ["/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", "/usr/share/fonts/TTF/DejaVuSans.ttf"]
BOLD_PATHS = ["/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf", "/usr/share/fonts/TTF/DejaVuSans-Bold.ttf"]

# Colours (OpenStreetMap-like, light).
LAND = (242, 239, 233)
OUTSIDE = (226, 223, 218)
GREEN = (205, 235, 176)
WATER = (170, 211, 223)
BUILDING = (217, 208, 201)
BUILDING_EDGE = (196, 182, 171)
HOSPITAL = (240, 196, 196)
SITE = (93, 64, 55)
RUBBLE = (161, 136, 127)
RUBBLE_EDGE = (78, 52, 46)
CLOSED = (211, 47, 47)
CASING = (176, 168, 160)
TEXT = (51, 51, 51)
HALO = (255, 255, 255)
CP = (25, 118, 210)

# Street styles: width in metres, minimum width in pixels, fill, casing, label size (px), from zoom.
STYLE = {
    "motorway": (16, 3.0, (232, 146, 162), (196, 100, 120), 13, 14),
    "trunk": (15, 3.0, (249, 178, 156), (200, 120, 100), 13, 14),
    "primary": (14, 2.6, (252, 214, 164), (198, 152, 92), 13, 14),
    "secondary": (12, 2.2, (247, 250, 191), (164, 168, 98), 12, 15),
    "tertiary": (10, 1.8, (255, 255, 255), CASING, 12, 15),
    "unclassified": (8, 1.4, (255, 255, 255), CASING, 11, 16),
    "residential": (8, 1.4, (255, 255, 255), CASING, 11, 16),
    "road": (8, 1.4, (255, 255, 255), CASING, 11, 16),
    "living_street": (7, 1.2, (237, 237, 237), CASING, 11, 17),
    "pedestrian": (7, 1.2, (221, 221, 232), (170, 170, 190), 11, 17),
    "service": (5, 1.0, (255, 255, 255), CASING, 10, 18),
    "track": (3, 0.8, (172, 131, 39), None, 10, 18),
    "footway": (2.5, 0.8, (250, 128, 114), None, 10, 18),
    "path": (2.5, 0.8, (250, 128, 114), None, 10, 18),
    "steps": (2.5, 0.8, (250, 128, 114), None, 10, 18),
    "cycleway": (2.5, 0.8, (0, 0, 255), None, 10, 18),
    "bridleway": (2.5, 0.8, (0, 128, 0), None, 10, 18),
    "corridor": (2.5, 0.8, (250, 128, 114), None, 10, 18),
}
DASHED = {"track", "footway", "path", "steps", "cycleway", "bridleway", "corridor"}


def style(cls):
    return STYLE.get(cls.replace("_link", ""), STYLE["residential"])


# --- Web Mercator ---------------------------------------------------------------------


def merc(lat, lon):
    """Normalised Web Mercator (0..1, y down)."""
    x = (lon + 180.0) / 360.0
    s = math.sin(math.radians(max(-85.0, min(85.0, lat))))
    y = 0.5 - math.log((1 + s) / (1 - s)) / (4 * math.pi)
    return x, y


def metres_per_px(lat, z):
    return 156543.03392 * math.cos(math.radians(lat)) / (2 ** z)


def tile_range(bounds, z):
    """XYZ tiles covering bounds (min lat, min lon, max lat, max lon)."""
    x0, y0 = merc(bounds[2], bounds[1])
    x1, y1 = merc(bounds[0], bounds[3])
    n = 2 ** z
    return int(x0 * n), int(x1 * n), int(y0 * n), int(y1 * n)


# --- Layers -----------------------------------------------------------------------------


class Feature:
    __slots__ = ("kind", "pts", "box", "props")

    def __init__(self, kind, latlons, props=None):
        self.kind = kind
        self.pts = [merc(p[0], p[1]) for p in latlons]
        xs = [p[0] for p in self.pts]
        ys = [p[1] for p in self.pts]
        self.box = (min(xs), min(ys), max(xs), max(ys))
        self.props = props or {}


L = None  # the layers, loaded once per process
FEATURES = []
GRID = {}  # (cx, cy) cell at zoom GRID_Z -> features touching it
GRID_Z = 15
LABELS = {}  # zoom -> list of (x, y, angle, text, size, bold) in global pixels
FONTS = {}
ARGS = None


def load(path):
    global L, FEATURES
    with open(path, encoding="utf-8") as f:
        L = json.load(f)
    feats = []
    for a in L.get("green") or []:
        feats.append(Feature("green", a["pts"]))
    for a in L.get("water") or []:
        feats.append(Feature("water", a["pts"]))
    for r in L.get("rivers") or []:
        feats.append(Feature("river", r))
    for a in L.get("buildings") or []:
        feats.append(Feature("building", a["pts"], {"name": a.get("name", ""), "kind": a.get("kind", "")}))
    for a in L.get("rubble") or []:
        feats.append(Feature("rubble", a["pts"]))
    for s in L.get("sites") or []:
        feats.append(Feature("site", s["pts"], s))
    for s in L.get("streets") or []:
        if len(s["pts"]) >= 2:
            feats.append(Feature("street", s["pts"], s))
    FEATURES = feats
    GRID.clear()
    n = 2 ** GRID_Z
    for f in feats:
        b = f.box
        for cx in range(int(b[0] * n) - 1, int(b[2] * n) + 2):
            for cy in range(int(b[1] * n) - 1, int(b[3] * n) + 2):
                GRID.setdefault((cx, cy), []).append(f)
    return L


def candidates(nx0, ny0, nx1, ny1, z):
    """Features that may touch the normalised box (the grid's margin covers wide lines)."""
    if z < GRID_Z:
        return FEATURES
    n = 2 ** GRID_Z
    seen, out = set(), []
    for cx in range(int(nx0 * n), int(nx1 * n) + 1):
        for cy in range(int(ny0 * n), int(ny1 * n) + 1):
            for f in GRID.get((cx, cy), ()):
                if id(f) not in seen:
                    seen.add(id(f))
                    out.append(f)
    return out


def font(size, bold=False):
    key = (size, bold)
    if key not in FONTS:
        paths = ([ARGS.bold_font] if bold and ARGS.bold_font else []) + ([ARGS.font] if not bold and ARGS.font else [])
        paths += BOLD_PATHS if bold else FONT_PATHS
        for p in paths:
            if p and os.path.exists(p):
                FONTS[key] = ImageFont.truetype(p, size)
                break
        else:
            # Pillow also looks through the system font folders by name. The
            # built-in bitmap font cannot be measured or centred on Pillow 9,
            # so no TrueType font is an error that says what to install.
            name = "DejaVuSans-Bold.ttf" if bold else "DejaVuSans.ttf"
            try:
                FONTS[key] = ImageFont.truetype(name, size)
            except OSError:
                sys.exit("render_map.py: E-MAP-004: font %s not found; install it with "
                         "'sudo apt install fonts-dejavu-core' or pass --font/--bold-font" % name)
    return FONTS[key]


# --- Labels: placed once per zoom in global pixels, so tiles agree ----------------------


def place_labels(z):
    scale = TILE * (2 ** z)
    lat0 = L["centre"][0]
    mpp = metres_per_px(lat0, z)
    placed = []  # boxes (x0, y0, x1, y1)
    out = []

    def free(box):
        for b in placed:
            if box[0] < b[2] and box[2] > b[0] and box[1] < b[3] and box[3] > b[1]:
                return False
        return True

    def take(box):
        placed.append(box)

    # Collapsed buildings first: they matter most.
    if z >= 16:
        for f in FEATURES:
            if f.kind != "site":
                continue
            p = f.props
            cx = sum(q[0] for q in f.pts[:-1]) / max(1, len(f.pts) - 1) * scale
            cy = sum(q[1] for q in f.pts[:-1]) / max(1, len(f.pts) - 1) * scale
            name = p.get("name") or p["id"]
            text = "%s collapse" % name if z < 18 else "%s: %s collapse, %.1f m" % (name, p["kind"], p["height_m"])
            size = 12 if z < 18 else 13
            w = font(size, True).getlength(text)
            y = cy - 6 - (size + 8) / 2 - max(6, 20 / mpp)
            box = (cx - w / 2 - 3, y - size / 2 - 3, cx + w / 2 + 3, y + size / 2 + 3)
            take(box)
            out.append((cx, y, 0.0, text, size, True, RUBBLE_EDGE))
    if z >= 15:
        cx, cy = merc(L["staging"][0], L["staging"][1])
        cx, cy = cx * scale, cy * scale
        text = "Command post"
        w = font(11, True).getlength(text)
        box = (cx + 8, cy - 9, cx + 12 + w, cy + 9)
        take(box)
        out.append((cx + 10 + w / 2, cy, 0.0, text, 11, True, CP))

    streets = [f for f in FEATURES if f.kind == "street" and f.props.get("name")]
    streets.sort(key=lambda f: -STYLE_ORDER.get(f.props["cls"].replace("_link", ""), 0))
    near = {}  # name -> label centres already placed
    spacing = 420
    for f in streets:
        cls = f.props["cls"]
        st = style(cls)
        if z < st[5]:
            continue
        name = f.props["name"]
        size = st[4] if z < 18 else st[4] + 1
        bold = st[4] >= 13
        pts = [(x * scale, y * scale) for x, y in f.pts]
        seg = []
        total = 0.0
        for i in range(1, len(pts)):
            d = math.hypot(pts[i][0] - pts[i - 1][0], pts[i][1] - pts[i - 1][1])
            seg.append(d)
            total += d
        w = font(size, bold).getlength(name)
        if total < w + 24:
            continue
        n = max(1, int(total // spacing))
        for k in range(n):
            s = total * (k + 0.5) / n
            a, b = along(pts, seg, s - w / 2 - 4), along(pts, seg, s + w / 2 + 4)
            mid = along(pts, seg, s)
            if not straight(pts, seg, s - w / 2, s + w / 2, a, b, size * 0.35):
                continue
            ang = math.degrees(math.atan2(-(b[1] - a[1]), b[0] - a[0]))
            if ang > 90:
                ang -= 180
            elif ang <= -90:
                ang += 180
            if any(math.hypot(mid[0] - q[0], mid[1] - q[1]) < spacing * 0.8 for q in near.get(name, [])):
                continue
            hw, hh = w / 2 + 4, size / 2 + 3
            c, sn = abs(math.cos(math.radians(ang))), abs(math.sin(math.radians(ang)))
            ex, ey = hw * c + hh * sn, hw * sn + hh * c
            box = (mid[0] - ex, mid[1] - ey, mid[0] + ex, mid[1] + ey)
            if not free(box):
                continue
            take(box)
            near.setdefault(name, []).append(mid)
            out.append((mid[0], mid[1], ang, name, size, bold, TEXT))
    # Named hospitals and places of worship at street level.
    if z >= 17:
        for f in FEATURES:
            if f.kind != "building" or f.props.get("kind") not in ("hospital", "worship") or not f.props.get("name"):
                continue
            cx = (f.box[0] + f.box[2]) / 2 * scale
            cy = (f.box[1] + f.box[3]) / 2 * scale
            text = ("+ " if f.props["kind"] == "hospital" else "") + f.props["name"]
            w = font(10, True).getlength(text)
            box = (cx - w / 2 - 3, cy - 8, cx + w / 2 + 3, cy + 8)
            if free(box):
                take(box)
                out.append((cx, cy, 0.0, text, 10, True, CLOSED if f.props["kind"] == "hospital" else TEXT))
    return out


STYLE_ORDER = {"motorway": 9, "trunk": 8, "primary": 7, "secondary": 6, "tertiary": 5, "unclassified": 4, "residential": 4,
               "road": 4, "living_street": 3, "pedestrian": 3, "service": 2}


def along(pts, seg, s):
    s = max(0.0, s)
    for i, d in enumerate(seg):
        if s <= d or i == len(seg) - 1:
            t = 0 if d == 0 else min(1.0, s / d)
            return (pts[i][0] + (pts[i + 1][0] - pts[i][0]) * t, pts[i][1] + (pts[i + 1][1] - pts[i][1]) * t)
        s -= d
    return pts[-1]


def straight(pts, seg, s0, s1, a, b, tol):
    """Whether the polyline between s0 and s1 stays within tol of the chord a-b."""
    dx, dy = b[0] - a[0], b[1] - a[1]
    ln = math.hypot(dx, dy)
    if ln < 1:
        return False
    for k in range(9):
        p = along(pts, seg, s0 + (s1 - s0) * k / 8)
        if abs((p[0] - a[0]) * dy - (p[1] - a[1]) * dx) / ln > tol:
            return False
    return True


# --- Drawing a tile -------------------------------------------------------------------------


def render_tile(job):
    z, tx, ty = job
    scale = TILE * (2 ** z)
    size = (TILE + 2 * MARGIN) * SS
    ox, oy = tx * TILE - MARGIN, ty * TILE - MARGIN
    lat0 = L["centre"][0]
    mpp = metres_per_px(lat0, z)
    im = Image.new("RGB", (size, size), LAND)
    d = ImageDraw.Draw(im, "RGBA")
    nx0, ny0 = ox / scale, oy / scale
    nx1, ny1 = (ox + TILE + 2 * MARGIN) / scale, (oy + TILE + 2 * MARGIN) / scale
    pad = 40 / mpp / scale  # features up to 40 m wide beyond their outline

    def visible(f):
        b = f.box
        return b[2] >= nx0 - pad and b[0] <= nx1 + pad and b[3] >= ny0 - pad and b[1] <= ny1 + pad

    def px(pts):
        return [((x * scale - ox) * SS, (y * scale - oy) * SS) for x, y in pts]

    feats = [f for f in candidates(nx0, ny0, nx1, ny1, z) if visible(f)]
    by = {}
    for f in feats:
        by.setdefault(f.kind, []).append(f)

    for f in by.get("green", []):
        d.polygon(px(f.pts), fill=GREEN)
    for f in by.get("water", []):
        d.polygon(px(f.pts), fill=WATER)
    for f in by.get("river", []):
        w = max(3, 28 / mpp) * SS
        line(d, px(f.pts), WATER, w)
    if z >= 15:
        for f in by.get("building", []):
            fill = HOSPITAL if f.props.get("kind") == "hospital" else BUILDING
            d.polygon(px(f.pts), fill=fill, outline=BUILDING_EDGE if z >= 17 else None)
    # Rubble spill areas, then the collapsed buildings in them.
    for f in by.get("rubble", []):
        hatch(im, px(f.pts), RUBBLE + (220,), RUBBLE_EDGE, max(4, 8 / mpp) * SS if z >= 16 else 0, SS)
    for f in by.get("site", []):
        d.polygon(px(f.pts), fill=SITE, outline=RUBBLE_EDGE)

    streets = by.get("street", [])
    widths = {}
    for f in streets:
        st = style(f.props["cls"])
        w = max(st[1], st[0] / mpp)
        if z <= 14 and f.props["cls"].replace("_link", "") in DASHED:
            w = 0
        widths[id(f)] = w * SS
    # Casings, then fills, small streets first (the layers file is sorted so).
    for f in streets:
        st = style(f.props["cls"])
        w = widths[id(f)]
        if w <= 0:
            continue
        cas = CLOSED if f.props.get("state") == "rubble" else st[3]
        if f.props.get("bridge"):
            cas = (90, 90, 90)
        if cas is not None and w >= 2.5 * SS and f.props["cls"].replace("_link", "") not in DASHED:
            line(d, px(f.pts), cas, w + max(1.0, w * 0.18) * 2)
    for f in streets:
        st = style(f.props["cls"])
        w = widths[id(f)]
        if w <= 0:
            continue
        pts = px(f.pts)
        state = f.props.get("state", "")
        fill = st[2]
        if state == "rubble":
            fill = (239, 154, 154)
        if f.props["cls"].replace("_link", "") in DASHED:
            # Paths: a light band at street level, a thin dashed line on it.
            if z >= 17:
                line(d, pts, (255, 255, 255, 150), w)
            dashes(d, pts, fill, max(1.2, min(2.0, w / SS)) * SS, 5 * SS, 3 * SS)
        else:
            if state == "closed":
                fill = (224, 224, 224)
            line(d, pts, fill, w)
        if state == "rubble":
            line(d, pts, CLOSED, max(1.5 * SS, w * 0.35))
        elif state == "closed":
            dashes(d, pts, CLOSED, max(1.5 * SS, w * 0.3), 7 * SS, 5 * SS)
    # The command post.
    if z >= 14:
        cx, cy = merc(L["staging"][0], L["staging"][1])
        x, y = (cx * scale - ox) * SS, (cy * scale - oy) * SS
        r = 5 * SS
        d.rectangle((x - r, y - r, x + r, y + r), fill=CP, outline=(255, 255, 255), width=SS)
    # Outside the downloaded area: fade.
    fade(im, ox, oy, scale, mpp)
    # Labels (global placement).
    for (lx, ly, ang, text, size, bold, colour) in LABELS.get(z, []):
        x, y = (lx - ox) * SS, (ly - oy) * SS
        if -300 * SS < x < im.width + 300 * SS and -60 * SS < y < im.height + 60 * SS:
            label(im, x, y, ang, text, size * SS, bold, colour)
    out = im.resize((TILE + 2 * MARGIN, TILE + 2 * MARGIN), RESAMPLE.LANCZOS)
    out = out.crop((MARGIN, MARGIN, MARGIN + TILE, MARGIN + TILE))
    path = os.path.join(ARGS.out, str(z), str(tx), "%d.png" % ty)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    out.save(path, optimize=True)
    return job


def line(d, pts, colour, w):
    if len(pts) < 2:
        return
    d.line(pts, fill=colour, width=max(1, int(round(w))), joint="curve")
    r = w / 2
    if r >= 1.5:
        for p in (pts[0], pts[-1]):
            d.ellipse((p[0] - r, p[1] - r, p[0] + r, p[1] + r), fill=colour)


def dashes(d, pts, colour, w, on, off):
    pos, draw_on, left = 0.0, True, on
    for i in range(1, len(pts)):
        a, b = pts[i - 1], pts[i]
        segl = math.hypot(b[0] - a[0], b[1] - a[1])
        t = 0.0
        while t < segl:
            step = min(left, segl - t)
            if draw_on:
                p0 = (a[0] + (b[0] - a[0]) * t / segl, a[1] + (b[1] - a[1]) * t / segl)
                p1 = (a[0] + (b[0] - a[0]) * (t + step) / segl, a[1] + (b[1] - a[1]) * (t + step) / segl)
                d.line([p0, p1], fill=colour, width=max(1, int(round(w))))
            t += step
            left -= step
            if left <= 1e-6:
                draw_on = not draw_on
                left = on if draw_on else off
        pos += segl


def hatch(im, pts, fill, edge, gap, ss):
    if len(pts) < 3:
        return
    xs, ys = [p[0] for p in pts], [p[1] for p in pts]
    x0, y0, x1, y1 = int(min(xs)) - 2, int(min(ys)) - 2, int(max(xs)) + 3, int(max(ys)) + 3
    if x1 < 0 or y1 < 0 or x0 > im.width or y0 > im.height:
        return
    layer = Image.new("RGBA", im.size, (0, 0, 0, 0))
    ld = ImageDraw.Draw(layer)
    ld.polygon(pts, fill=fill)
    if gap > 0:
        mask = Image.new("L", im.size, 0)
        ImageDraw.Draw(mask).polygon(pts, fill=255)
        lines = Image.new("RGBA", im.size, (0, 0, 0, 0))
        hd = ImageDraw.Draw(lines)
        g = int(max(3, gap))
        for k in range(x0 - (y1 - y0), x1 + 1, g):
            hd.line([(k, y1), (k + (y1 - y0), y0)], fill=edge + (200,), width=max(1, ss))
        layer.paste(lines, (0, 0), Image.composite(lines, Image.new("RGBA", im.size, (0, 0, 0, 0)), mask).getchannel("A"))
    ld.line(pts + [pts[0]], fill=edge + (255,), width=max(1, ss), joint="curve")
    im.paste(layer, (0, 0), layer)


def fade(im, ox, oy, scale, mpp):
    """Fade what lies outside the downloaded area (a circle of the layers' radius)."""
    cx, cy = merc(L["centre"][0], L["centre"][1])
    cx, cy = (cx * scale - ox) * SS, (cy * scale - oy) * SS
    r = L["radius_m"] / mpp * SS
    corners = [(0, 0), (im.width, 0), (0, im.height), (im.width, im.height)]
    if all(math.hypot(x - cx, y - cy) < r for x, y in corners):
        return
    mask = Image.new("L", im.size, 150)
    ImageDraw.Draw(mask).ellipse((cx - r, cy - r, cx + r, cy + r), fill=0)
    im.paste(Image.new("RGB", im.size, OUTSIDE), (0, 0), mask)


def label(im, x, y, ang, text, size, bold, colour):
    f = font(int(size), bold)
    w = int(f.getlength(text)) + 8 * SS
    h = int(size * 1.6) + 4 * SS
    t = Image.new("RGBA", (w, h), (0, 0, 0, 0))
    td = ImageDraw.Draw(t)
    td.text((w / 2, h / 2), text, font=f, fill=colour + (255,), anchor="mm", stroke_width=max(2, int(size / 6)), stroke_fill=HALO + (230,))
    if abs(ang) > 0.5:
        t = t.rotate(ang, resample=RESAMPLE.BICUBIC, expand=True)
    im.paste(t, (int(x - t.width / 2), int(y - t.height / 2)), t)


# --- Legend, overview, MBTiles ------------------------------------------------------------


def legend(path):
    rows = [("street", None), ("closed by rubble", "rubble"), ("closed in OpenStreetMap", "closed"), ("footway", "foot"),
            ("collapsed building", "site"), ("rubble (closes streets)", "rubble_area"), ("river, water", "water"),
            ("building", "building"), ("command post", "cp")]
    W, H = 300 * SS, (len(rows) * 26 + 40) * SS
    im = Image.new("RGB", (W, H), (255, 255, 255))
    d = ImageDraw.Draw(im, "RGBA")
    d.text((12 * SS, 10 * SS), "RubbleSAR incident map", font=font(14 * SS, True), fill=TEXT)
    for i, (text, kind) in enumerate(rows):
        y = (40 + i * 26) * SS
        a, b = (14 * SS, y + 8 * SS), (60 * SS, y + 8 * SS)
        if kind in (None, "rubble", "closed"):
            line(d, [a, b], CASING if kind != "rubble" else CLOSED, 9 * SS)
            line(d, [a, b], (255, 255, 255) if kind != "rubble" else (239, 154, 154), 6 * SS)
            if kind == "rubble":
                line(d, [a, b], CLOSED, 2.5 * SS)
            if kind == "closed":
                dashes(d, [a, b], CLOSED, 2.5 * SS, 8 * SS, 6 * SS)
        elif kind == "foot":
            dashes(d, [a, b], (250, 128, 114), 2 * SS, 6 * SS, 4 * SS)
        elif kind == "site":
            d.rectangle((14 * SS, y, 60 * SS, y + 16 * SS), fill=SITE)
        elif kind == "rubble_area":
            hatch(im, [(14 * SS, y), (60 * SS, y), (60 * SS, y + 16 * SS), (14 * SS, y + 16 * SS)], RUBBLE + (220,), RUBBLE_EDGE, 8 * SS, SS)
            d = ImageDraw.Draw(im, "RGBA")
        elif kind == "water":
            d.rectangle((14 * SS, y, 60 * SS, y + 16 * SS), fill=WATER)
        elif kind == "building":
            d.rectangle((14 * SS, y, 60 * SS, y + 16 * SS), fill=BUILDING, outline=BUILDING_EDGE)
        elif kind == "cp":
            d.rectangle((32 * SS, y + 3 * SS, 42 * SS, y + 13 * SS), fill=CP)
        d.text((72 * SS, y + 8 * SS), text, font=font(12 * SS), fill=TEXT, anchor="lm")
    im.resize((W // SS, H // SS), RESAMPLE.LANCZOS).save(path)


def overview(path, z):
    """The whole area at zoom z, from the tiles."""
    x0, x1, y0, y1 = tile_range(L["bounds"], z)
    im = Image.new("RGB", ((x1 - x0 + 1) * TILE, (y1 - y0 + 1) * TILE), LAND)
    for x in range(x0, x1 + 1):
        for y in range(y0, y1 + 1):
            p = os.path.join(ARGS.out, str(z), str(x), "%d.png" % y)
            if os.path.exists(p):
                im.paste(Image.open(p), ((x - x0) * TILE, (y - y0) * TILE))
    im.save(path, optimize=True)
    meta = {"zoom": z, "x0": x0, "y0": y0, "width": im.width, "height": im.height}
    with open(path + ".json", "w") as f:
        json.dump(meta, f)


def mbtiles(path, jobs):
    tmp = path + ".tmp"
    if os.path.exists(tmp):
        os.remove(tmp)
    db = sqlite3.connect(tmp)
    db.execute("CREATE TABLE metadata (name TEXT, value TEXT)")
    db.execute("CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB)")
    b = L["bounds"]
    meta = {"name": "RubbleSAR %s" % L["incident"], "format": "png", "type": "baselayer", "version": "1",
            "description": "Offline incident map: streets as routed, closures, collapsed buildings (simulated)",
            "attribution": L.get("attribution", "Map data (c) OpenStreetMap contributors"),
            "bounds": "%.6f,%.6f,%.6f,%.6f" % (b[1], b[0], b[3], b[2]),
            "center": "%.6f,%.6f,%d" % (L["centre"][1], L["centre"][0], 16),
            "minzoom": str(ARGS.min_zoom), "maxzoom": str(ARGS.max_zoom)}
    db.executemany("INSERT INTO metadata VALUES (?, ?)", list(meta.items()))
    for z, x, y in jobs:
        with open(os.path.join(ARGS.out, str(z), str(x), "%d.png" % y), "rb") as f:
            db.execute("INSERT INTO tiles VALUES (?, ?, ?, ?)", (z, x, (2 ** z) - 1 - y, f.read()))
    db.execute("CREATE UNIQUE INDEX tile_index ON tiles (zoom_level, tile_column, tile_row)")
    db.commit()
    db.close()
    os.replace(tmp, path)


def init(args, labels):
    global ARGS, LABELS
    ARGS = args
    LABELS = labels
    load(args.layers)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--layers", required=True, help="rescue-dispatch map-layers output")
    ap.add_argument("--out", required=True, help="tile folder to write ({z}/{x}/{y}.png)")
    ap.add_argument("--mbtiles", default="", help="also write this MBTiles file")
    ap.add_argument("--min-zoom", type=int, default=13)
    ap.add_argument("--max-zoom", type=int, default=19)
    ap.add_argument("--jobs", type=int, default=max(1, (os.cpu_count() or 2) - 1))
    ap.add_argument("--font", default="")
    ap.add_argument("--bold-font", default="")
    ap.add_argument("--legend", default="", help="also write a legend image")
    ap.add_argument("--overview", default="", help="also write the whole area at zoom 16 as one image")
    args = ap.parse_args()
    if not (0 <= args.min_zoom <= args.max_zoom <= 21):
        sys.exit("E-MAP-003: zooms must satisfy 0 <= --min-zoom <= --max-zoom <= 21")
    global ARGS
    ARGS = args
    t0 = time.time()
    load(args.layers)
    labels = {z: place_labels(z) for z in range(args.min_zoom, args.max_zoom + 1)}
    jobs = []
    for z in range(args.min_zoom, args.max_zoom + 1):
        x0, x1, y0, y1 = tile_range(L["bounds"], z)
        jobs += [(z, x, y) for x in range(x0, x1 + 1) for y in range(y0, y1 + 1)]
    print("drawing %d tiles, zoom %d to %d, of %s (%d street runs, %d buildings, %d collapsed buildings) with %d processes ..."
          % (len(jobs), args.min_zoom, args.max_zoom, L["incident"], len(L.get("streets") or []), len(L.get("buildings") or []),
             len(L.get("sites") or []), args.jobs), flush=True)
    os.makedirs(args.out, exist_ok=True)
    done = 0
    if args.jobs > 1:
        with Pool(args.jobs, initializer=init, initargs=(args, labels)) as pool:
            for _ in pool.imap_unordered(render_tile, jobs, chunksize=8):
                done += 1
                if done % 500 == 0:
                    print("  %d of %d tiles" % (done, len(jobs)), flush=True)
    else:
        init(args, labels)
        for j in jobs:
            render_tile(j)
    if args.mbtiles:
        mbtiles(args.mbtiles, jobs)
    if args.legend:
        legend(args.legend)
    if args.overview:
        overview(args.overview, min(16, args.max_zoom))
    size = sum(os.path.getsize(os.path.join(dp, f)) for dp, _, fs in os.walk(args.out) for f in fs)
    print("wrote %d tiles (%.1f MB) to %s%s in %.0f s" % (len(jobs), size / 1e6, args.out,
          (" and " + args.mbtiles) if args.mbtiles else "", time.time() - t0))


if __name__ == "__main__":
    main()
