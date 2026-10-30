# Builds the landing page's block world map (cmd/argonmine-web/site/static/worldgrid.json)
# from Natural Earth's 1:110m admin-0 countries (public domain):
#   python -I scripts/worldgrid.py ne_110m_admin_0_countries.geojson cmd/argonmine-web/site/static/worldgrid.json
# Every 2-degree cell whose centre lies in a country becomes a block of that
# country (ISO 3166-1 alpha-2); a country too small to hold a cell centre
# gets the cell at its label point, so every listed country can light up.
import json
import sys

STEP = 2.0
LAT_TOP, LAT_BOTTOM = 82.0, -58.0  # Antarctica and the far north left out
COLS = int(360 / STEP)
ROWS = int((LAT_TOP - LAT_BOTTOM) / STEP)


# Countries too small for the 1:110m data, at their usual map point (lat, lon);
# each gets one block (over the sea, or over a neighbour's block).
SMALL = {
    "SG": (1.35, 103.82), "HK": (22.32, 114.17), "MO": (22.2, 113.55), "BH": (26.07, 50.55),
    "MT": (35.9, 14.45), "LU": (49.8, 6.1), "MU": (-20.2, 57.5), "MV": (3.2, 73.2),
    "SC": (-4.6, 55.45), "CV": (15.1, -23.6), "BB": (13.2, -59.55), "BM": (32.3, -64.8),
    "KY": (19.3, -81.25), "AW": (12.5, -69.97), "CW": (12.2, -69.0), "WS": (-13.8, -172.1),
    "TO": (-21.2, -175.2), "AG": (17.1, -61.8), "LC": (13.9, -60.98), "GD": (12.1, -61.7),
    "DM": (15.4, -61.35), "KN": (17.3, -62.7), "VC": (13.25, -61.2), "KM": (-11.7, 43.3),
    "ST": (0.3, 6.6), "FJ": (-17.7, 178.0), "MH": (7.1, 171.2), "FM": (6.9, 158.2), "PW": (7.5, 134.6),
}


def code_of(props):
    for k in ("ISO_A2_EH", "ISO_A2", "WB_A2"):
        v = str(props.get(k) or "")
        if len(v) == 2 and v.isalpha():
            return v.upper()
    return ""


def polygons(geom):
    if geom["type"] == "Polygon":
        return [geom["coordinates"]]
    if geom["type"] == "MultiPolygon":
        return geom["coordinates"]
    return []


def in_ring(x, y, ring):
    inside = False
    j = len(ring) - 1
    for i in range(len(ring)):
        xi, yi = ring[i][0], ring[i][1]
        xj, yj = ring[j][0], ring[j][1]
        if (yi > y) != (yj > y) and x < (xj - xi) * (y - yi) / (yj - yi) + xi:
            inside = not inside
        j = i
    return inside


def in_polygon(x, y, poly):
    if not in_ring(x, y, poly[0]):
        return False
    return not any(in_ring(x, y, hole) for hole in poly[1:])


def main(src, out):
    feats = json.load(open(src, encoding="utf-8"))["features"]
    shapes = []
    for f in feats:
        cc = code_of(f["properties"])
        if not cc:
            continue
        for poly in polygons(f["geometry"]):
            xs = [p[0] for p in poly[0]]
            ys = [p[1] for p in poly[0]]
            shapes.append((cc, min(xs), max(xs), min(ys), max(ys), poly))
    grid = [[""] * COLS for _ in range(ROWS)]
    for r in range(ROWS):
        lat = LAT_TOP - STEP * (r + 0.5)
        for c in range(COLS):
            lon = -180 + STEP * (c + 0.5)
            for cc, x0, x1, y0, y1, poly in shapes:
                if x0 <= lon <= x1 and y0 <= lat <= y1 and in_polygon(lon, lat, poly):
                    grid[r][c] = cc
                    break
    have = {cc for row in grid for cc in row if cc}
    for f in feats:
        cc = code_of(f["properties"])
        p = f["properties"]
        if not cc or cc in have or p.get("LABEL_X") is None:
            continue
        c = int((float(p["LABEL_X"]) + 180) / STEP)
        r = int((LAT_TOP - float(p["LABEL_Y"])) / STEP)
        if 0 <= r < ROWS and 0 <= c < COLS:
            grid[r][c] = cc
            have.add(cc)
    for cc, (lat, lon) in SMALL.items():
        if cc in have:
            continue
        c = int((lon + 180) / STEP)
        r = int((LAT_TOP - lat) / STEP)
        if 0 <= r < ROWS and 0 <= c < COLS:
            grid[r][c] = cc
            have.add(cc)
    codes = sorted(have)
    index = {cc: i for i, cc in enumerate(codes)}
    # Each row as runs [start column, length, country index].
    runs = []
    for row in grid:
        rr, c = [], 0
        while c < COLS:
            if not row[c]:
                c += 1
                continue
            s, cc = c, row[c]
            while c < COLS and row[c] == cc:
                c += 1
            rr.append([s, c - s, index[cc]])
        runs.append(rr)
    doc = {"source": "Natural Earth 1:110m admin-0 countries (public domain)", "step": STEP, "cols": COLS, "rows": ROWS,
           "lat_top": LAT_TOP, "codes": codes, "runs": runs}
    open(out, "w", encoding="utf-8", newline="\n").write(json.dumps(doc, separators=(",", ":")) + "\n")
    print(f"{len(codes)} countries, {sum(len(r) for r in runs)} runs, {sum(1 for row in grid for x in row if x)} cells")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
