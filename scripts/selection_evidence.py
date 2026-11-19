#!/usr/bin/env python3
"""Prove selection semantics against a real PNG.

Fetches the canonical PNG for the pick-proof scene, decodes it with the Python
standard library only, and checks that the pick endpoint's explanation agrees
with pixels that were actually rendered:

  * hole centre: the enclosing outline is not selectable, underlying paint is;
  * layer clip: a shape whose fill covers the point but whose layer excludes it
    does not paint, and is reported as clipped rather than invisible;
  * low opacity: a 5% veil remains selectable and measurably changes the PNG;
  * transparent gradient end: the gradient shape owns the pixel at its opaque
    end, and its contribution falls to zero at the fully transparent end;
  * occlusion: opaque later paint covers an earlier shape, which is reported as
    covered rather than selected;
  * joint blend: the click is attributed to an ordered list, not a single object.

Exit status is non-zero if any expectation fails. The script writes the PNG and
an annotated copy to a caller-provided directory for visual inspection.
"""
import argparse, json, pathlib, shutil, socket, subprocess, sys, tempfile, time
import urllib.error, urllib.request
import zlib, struct

ROOT = pathlib.Path(__file__).resolve().parents[1]
SEED = ROOT / "examples" / "selection-proof.json"


def decode_png(raw):
    """Minimal PNG decoder for the 8-bit RGB/RGBA non-interlaced proof output."""
    assert raw[:8] == b"\x89PNG\r\n\x1a\n", "not a PNG"
    pos, idat = 8, b""
    width = height = ctype = depth = None
    while pos < len(raw):
        n = struct.unpack(">I", raw[pos:pos + 4])[0]
        kind = raw[pos + 4:pos + 8]
        data = raw[pos + 8:pos + 8 + n]
        if kind == b"IHDR":
            width, height, depth, ctype = struct.unpack(">IIBB", data[:10])
        elif kind == b"IDAT":
            idat += data
        pos += 12 + n
    assert depth == 8 and ctype in (2, 6) and width and height
    bpp = 4 if ctype == 6 else 3
    stride = width * bpp
    raw_px = zlib.decompress(idat)
    rows, prev, i = [], bytearray(stride), 0
    for _ in range(height):
        f = raw_px[i]
        i += 1
        line = bytearray(raw_px[i:i + stride])
        i += stride
        for x in range(stride):
            a = line[x - bpp] if x >= bpp else 0
            b = prev[x]
            c = prev[x - bpp] if x >= bpp else 0
            if f == 1:
                line[x] = (line[x] + a) & 255
            elif f == 2:
                line[x] = (line[x] + b) & 255
            elif f == 3:
                line[x] = (line[x] + (a + b) // 2) & 255
            elif f == 4:
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                pr = a if pa <= pb and pa <= pc else (b if pb <= pc else c)
                line[x] = (line[x] + pr) & 255
        rows.append(bytes(line))
        prev = line
    return width, height, bpp, rows


def pixel(img, x, y):
    width, height, bpp, rows = img
    if not (0 <= x < width and 0 <= y < height):
        raise AssertionError(f"pixel {x},{y} outside {width}x{height}")
    off = x * bpp
    return tuple(rows[y][off:off + 3])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="", help="directory to keep PNG evidence in")
    ap.add_argument("--port", type=int, default=0)
    args = ap.parse_args()

    probe = socket.socket()
    probe.bind(("127.0.0.1", 0))
    port = args.port or probe.getsockname()[1]
    probe.close()

    with tempfile.TemporaryDirectory(prefix="prism-pick-") as tmp:
        base = pathlib.Path(tmp)
        binary = base / "prismplate"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/prismplate"],
                       cwd=ROOT, check=True)
        log = (base / "service.log").open("w")
        proc = subprocess.Popen(
            [str(binary), "-listen", f"127.0.0.1:{port}", "-data", str(base / "state")],
            stdout=log, stderr=log)
        url = f"http://127.0.0.1:{port}"

        def stop():
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=45)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()

        def call(method, path, data=None, headers=None, raw=False):
            body = json.dumps(data).encode() if data is not None else None
            req = urllib.request.Request(
                url + path, data=body, method=method,
                headers=headers or {"Content-Type": "application/json"})
            try:
                with urllib.request.urlopen(req, timeout=30) as r:
                    payload, code = r.read(), r.status
            except urllib.error.HTTPError as e:
                payload, code = e.read(), e.code
            if code != 200 and code != 201:
                raise AssertionError(f"{method} {path} -> {code}: {payload[:200]!r}")
            return payload if raw else json.loads(payload)

        try:
            for _ in range(200):
                if proc.poll() is not None:
                    raise RuntimeError("service exited during startup")
                try:
                    urllib.request.urlopen(url + "/healthz", timeout=1).close()
                    break
                except OSError:
                    time.sleep(0.05)
            call("POST", "/v1/plates", json.loads(SEED.read_text()), headers={"Content-Type": "application/json"})
            artifact = call("POST", "/v1/plates/selection-proof/render?revision=1&format=png")
            raw = call("GET", "/v1/artifacts/" + artifact["id"], raw=True,
                       headers={"Accept": "image/png"})
            img = decode_png(raw)

            failures = []

            def hit(x, y, options=None):
                body = {"x": x, "y": y}
                if options:
                    body["options"] = options
                return call("POST", f"/v1/plates/selection-proof/hit?revision=1", body)

            def check(name, ok, detail):
                print(("PASS " if ok else "FAIL ") + name + ": " + detail)
                if not ok:
                    failures.append(name)

            # --- holes -------------------------------------------------------
            # (65,25) is inside the ring's outer square but inside its inner hole.
            h = hit(65, 25)
            ring = next(c for c in h["candidates"] if c["shape_id"] == "ring")
            check("hole-not-selectable", ring["hole"] and not ring["selectable"]
                  and ring["occlusion"] == "hole",
                  f"ring hole={ring['hole']} selectable={ring['selectable']} occlusion={ring['occlusion']}")
            check("hole-shows-underlay", h["selected"] == "under-plate",
                  f"selected={h['selected']} rgb={pixel(img, 65, 25)}")
            check("hole-pixel-is-underlay", pixel(img, 65, 25) == pixel(img, 15, 85),
                  f"hole rgb={pixel(img, 65, 25)} underlay rgb={pixel(img, 15, 85)}")

            # The ring band itself is selectable and visible.
            h = hit(45, 25)
            check("ring-band-selectable", h["selected"] == "ring",
                  f"selected={h['selected']} rgb={pixel(img, 45, 25)}")

            # --- layer clip --------------------------------------------------
            # cover-block's fill spans x 50..130, but its layer clip ends at
            # x=100. At (115,60) the fill covers the point and nothing paints.
            h = hit(115, 60)
            cover = [c for c in h["candidates"] if c["shape_id"] == "cover-block"]
            check("clip-reports-clipped", bool(cover) and cover[0]["layer_clip"] in ("clipped", "masked")
                  and not cover[0]["selectable"] and cover[0]["occlusion"] == "clipped",
                  f"cover={cover[0]['layer_clip'] if cover else None} occlusion={cover[0]['occlusion'] if cover else None} selectable={cover[0]['selectable'] if cover else None}")
            check("clip-pixel-is-background", pixel(img, 115, 60) == pixel(img, 5, 5),
                  f"clipped rgb={pixel(img, 115, 60)} background rgb={pixel(img, 5, 5)}")
            check("clip-not-selected", h.get("selected") is None, f"selected={h.get('selected')}")
            # Away from any fill, a clipped-layer shape is not even evidence.
            h = hit(100, 35)
            check("clip-excluded-not-listed",
                  all(c["shape_id"] != "cover-block" for c in h["candidates"]),
                  f"candidates={[c['shape_id'] for c in h['candidates']]}")

            # --- low opacity -------------------------------------------------
            # veil-band is 5% white over under-plate at (30,70). Under the tiny
            # default floor it is "visible" and selectable; raising the floor makes
            # it faint evidence without changing the PNG.
            h = hit(30, 70)
            veil = next(c for c in h["candidates"] if c["shape_id"] == "veil-band")
            base_px = pixel(img, 30, 40)  # under-plate without veil
            veil_px = pixel(img, 30, 70)
            check("faint-still-selectable", veil["selectable"]
                  and veil["contribution"] > 0,
                  f"selectable={veil['selectable']} occlusion={veil['occlusion']} contrib={veil['contribution']}")
            check("faint-changes-png", veil_px != base_px,
                  f"veil rgb={veil_px} base rgb={base_px}")
            check("faint-not-unique", veil_px[0] > base_px[0] and h["ambiguous"] and len(h["ambiguous"]) > 1,
                  f"ambiguous={h['ambiguous']}")

            # Raising min_contribution must not change what the PNG shows, only
            # the selectable set; the veil becomes faint, not selectable.
            strict = hit(30, 70, {"min_contribution": 0.05})
            veil_strict = next(c for c in strict["candidates"] if c["shape_id"] == "veil-band")
            check("min-contribution-tunable", not veil_strict["selectable"]
                  and veil_strict["occlusion"] == "faint",
                  f"selectable={veil_strict['selectable']} occlusion={veil_strict['occlusion']} contrib={veil_strict['contribution']}")

            # --- transparent gradient ---------------------------------------
            # fade-bar is a horizontal gradient with alpha 0 at x=20 and 1 at x=140.
            h = hit(135, 105)
            check("gradient-opaque-end-selected", h["selected"] == "fade-bar",
                  f"selected={h['selected']} alpha={h['candidates'][0]['local_alpha']}")
            h = hit(22, 105)
            fade = next(c for c in h["candidates"] if c["shape_id"] == "fade-bar")
            check("gradient-transparent-end-faint", fade["contribution"] < 0.01,
                  f"occlusion={fade['occlusion']} contrib={fade['contribution']} alpha={fade['local_alpha']}")
            # The gradient is transparent enough near its start that the pixel is
            # indistinguishable from the underlay at x=22 (5% opacity of 1/6 alpha).
            check("gradient-end-pixel-near-underlay",
                  pixel(img, 22, 105) != pixel(img, 30, 105) and
                  abs(pixel(img, 22, 105)[0] - pixel(img, 30, 105)[0]) < 30,
                  f"x22 rgb={pixel(img, 22, 105)} x30 rgb={pixel(img, 30, 105)}")
            # --- occlusion ---------------------------------------------------
            h = hit(60, 60)
            under = next(c for c in h["candidates"] if c["shape_id"] == "under-plate")
            check("covered-not-selected", not under["selectable"] and under["occlusion"] == "covered",
                  f"selectable={under['selectable']} occlusion={under['occlusion']} occluded_by={under.get('occluded_by')}")
            # The cover block plus the faint veil produce the pixel at (60,60);
            # neither equals the bare background or the uncovered underlay.
            check("covered-pixel-is-not-underlay",
                  pixel(img, 60, 60) != pixel(img, 30, 40),
                  f"covered-point rgb={pixel(img, 60, 60)} underlay rgb={pixel(img, 30, 40)}")
            check("top-is-cover", h["selected"] == "cover-block",
                  f"selected={h['selected']}")

            # Explicit pick-through is the only way to select covered paint.
            through = hit(60, 60, {"include_covered": True})
            under2 = next(c for c in through["candidates"] if c["shape_id"] == "under-plate")
            check("pick-through-requires-opt-in", under2["selectable"] and under2["occlusion"] == "covered",
                  f"selectable={under2['selectable']}")

            # --- edge tolerance ---------------------------------------------
            # The ring's outer edge is at x=85; a click just outside it at x=86
            # is selectable only within edge tolerance.
            near = hit(86, 25)
            ring_near = next((c for c in near["candidates"] if c["shape_id"] == "ring"), None)
            tight = hit(86, 25, {"edge_tolerance_px": 0.25})
            ring_tight = next((c for c in tight["candidates"] if c["shape_id"] == "ring"), None)
            check("edge-tolerance-affects-selection",
                  ring_near is not None and ring_near["edge"] and ring_near["selectable"]
                  and (ring_tight is None or not ring_tight["selectable"]),
                  f"default={ring_near['edge_distance_px'] if ring_near else None} tight={ring_tight['edge_distance_px'] if ring_tight else None}")

            # --- options are echoed and bounded ------------------------------
            check("options-echoed", near["options"]["edge_tolerance_px"] == 1.5
                  and near["options"]["joint_share"] == 0.75,
                  f"options={near['options']}")
            bad = urllib.request.Request(
                url + "/v1/plates/selection-proof/hit?revision=1",
                data=b'{"x":1,"y":1,"options":{"max_candidates":99}}',
                method="POST", headers={"Content-Type": "application/json"})
            try:
                urllib.request.urlopen(bad, timeout=5)
                code = 200
            except urllib.error.HTTPError as e:
                code = e.code
            check("invalid-options-rejected", code == 400, f"status={code}")

            # --- annotated evidence -----------------------------------------
            if args.out:
                out = pathlib.Path(args.out)
                out.mkdir(parents=True, exist_ok=True)
                (out / "selection-proof.png").write_bytes(raw)
                shutil.copy(SEED, out / "selection-proof.json")
                points = {
                    "hole": (65, 25), "ring": (45, 25), "clip": (115, 60),
                    "cover": (60, 60), "veil": (30, 70), "gradient": (135, 105),
                    "empty": (100, 35),
                }
                annotate(img, points, out / "selection-proof-annotated.png")
                (out / "evidence.json").write_text(json.dumps({
                    "artifact": artifact,
                    "png_pixels": {k: pixel(img, x, y) for k, (x, y) in points.items()},
                    "hits": {k: compact(hit(x, y)) for k, (x, y) in points.items()},
                }, ensure_ascii=False, indent=1))

            if failures:
                print("FAILED " + ", ".join(failures))
                return 1
            print("selection evidence passed")
            return 0
        finally:
            stop()
            log.close()


def compact(result):
    return {
        "selected": result.get("selected"),
        "ambiguous": result.get("ambiguous"),
        "note": result.get("note"),
        "candidates": [
            {
                "shape": c["shape_id"], "layer": c["layer_id"],
                "inside": c["inside"], "hole": c["hole"], "edge": c["edge"],
                "occlusion": c["occlusion"], "selectable": c["selectable"],
                "contribution": c["contribution"], "score": c["score"],
            }
            for c in result["candidates"]
        ],
    }


def annotate(img, points, path):
    """Draw 5x5 markers directly into a decoded RGB image and re-encode."""
    width, height, bpp, rows = img
    out = [bytearray(r) for r in rows]
    colors = {
        "hole": (0, 255, 255), "ring": (0, 255, 0), "clip": (255, 0, 255),
        "cover": (255, 255, 0), "veil": (255, 128, 0), "gradient": (255, 0, 0),
        "empty": (128, 128, 255),
    }
    for name, (x, y) in points.items():
        r, g, b = colors[name]
        for dy in range(-3, 4):
            for dx in range(-3, 4):
                if max(abs(dx), abs(dy)) < 2:
                    continue
                px, py = x + dx, y + dy
                if 0 <= px < width and 0 <= py < height:
                    off = px * bpp
                    out[py][off:off + 3] = bytes((r, g, b))
    raw = bytearray()
    for row in out:
        raw.append(0)
        raw += row
    def chunk(kind, data):
        return (struct.pack(">I", len(data)) + kind + data
                + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF))
    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    png = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header)
           + chunk(b"IDAT", zlib.compress(bytes(raw), 9)) + chunk(b"IEND", b""))
    path.write_bytes(png)


if __name__ == "__main__":
    sys.exit(main())
