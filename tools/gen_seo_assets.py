#!/usr/bin/env python3
"""
Generate the co:op website's SEO / social assets (site/assets/img/*).

No third-party Python deps — the stdlib plus two CLIs already on the box:

  • headless Google Chrome   rasterizes SVG/HTML crisply
  • ImageMagick (`magick`)   downscales the master renders and bundles favicon.ico

The icons are drawn from the brand's mark, brand/assets/coop-flat.svg, and the social card is the
homepage's own first screen, so neither can drift from the site. Re-run after changing the mark or
the homepage's hero (regenerate site/index.html first: python3 tools/gen_site.py).

  Outputs (all committed):
    favicon.svg              the mark, scalable — primary icon for modern browsers
    favicon.ico              16/32/48 multi-res — legacy browsers + Google results
    apple-touch-icon.png     180, opaque full-bleed — iOS home screen
    icon-192.png             192, manifest "any"
    icon-512.png             512, manifest "any"
    icon-maskable-512.png    512, full-bleed, glyph in the safe zone — Android adaptive
    og-image.png             1200x630 — Open Graph + Twitter summary_large_image

Usage:  python3 tools/gen_seo_assets.py            # regenerate everything
        python3 tools/gen_seo_assets.py icons      # just the favicon/app icons
        python3 tools/gen_seo_assets.py og         # just the social card

Chrome path: $CHROME, else the macOS default, else chromium/google-chrome on PATH.
"""


import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
IMG = ROOT / "site" / "assets" / "img"

# The mark: a dark rounded tile, the wood house, the cyan loop-eye. Every icon is drawn from it.
FAVICON_SVG = (ROOT / "brand" / "assets" / "coop-flat.svg").read_text()
TILE = re.search(r'<rect [^>]*fill="(#[0-9A-Fa-f]{6})"', FAVICON_SVG).group(1)
GLYPH = "".join(re.findall(r"<(?:path|circle) [^>]*/>", FAVICON_SVG))

# Full-bleed (no rounded corners) — the OS rounds it. Base for apple-touch.
FULLBLEED_SVG = f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" fill="{TILE}"/>{GLYPH}</svg>'

# Maskable — full-bleed tile, glyph scaled into the central safe zone (Android masks
# to a circle of ~80% diameter; keep content well inside it).
MASKABLE_SVG = (
    f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" fill="{TILE}"/>'
    f'<g transform="translate(16 16) scale(0.64) translate(-16 -16)">{GLYPH}</g></svg>'
)

# The social card: the homepage's first screen at 1372x720 (resized to 1200x630), without its
# scripts, so the sandbox picture is finished; the nav links, the buttons and everything below the
# hero are left out.
OG_STYLE = """<style>
  .nav-links { visibility: hidden; }
  .actions, .story-steps, main > section:not(.story), footer { display: none; }
  .story { padding-top: 8px; }
  .story-lead { min-height: 0; }
  .stage { grid-row: 1; height: auto; }
</style>"""


def find_chrome():
    env = os.environ.get("CHROME")
    if env and Path(env).exists():
        return env
    mac = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    if Path(mac).exists():
        return mac
    for name in ("google-chrome", "google-chrome-stable", "chromium", "chromium-browser"):
        p = shutil.which(name)
        if p:
            return p
    sys.exit("error: Google Chrome / Chromium not found. Set $CHROME to its path.")


def need_magick():
    if not shutil.which("magick"):
        sys.exit("error: ImageMagick `magick` not found (brew install imagemagick).")
    return "magick"


def shoot(chrome, url, px, out, *, scale=1, page=False):
    """Headless-screenshot `url` into a px*scale square (or WxH for the OG page). A page may load
    its stylesheet and fonts from the site's files."""
    if isinstance(px, tuple):
        w, h = px
    else:
        w = h = px
    flags = ["--allow-file-access-from-files"] if page else []
    subprocess.run(
        [
            chrome, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-sandbox", *flags,
            f"--force-device-scale-factor={scale}", f"--window-size={w},{h}",
            "--default-background-color=00000000", f"--screenshot={out}", url,
        ],
        check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )


def render_svg_master(chrome, tmp, svg_text, name, px=1024):
    """Render an SVG to a high-res master PNG (Chrome won't paint sub-min windows,
    so we always render large and let ImageMagick downscale)."""
    html = tmp / f"{name}.html"
    html.write_text(
        "<!doctype html><meta charset=utf-8>"
        "<style>html,body{margin:0;padding:0}svg{width:100vw;height:100vh;display:block}</style>"
        + svg_text
    )
    master = tmp / f"{name}.png"
    shoot(chrome, html.as_uri(), px, str(master))
    return master


def resize(magick, src, px, out):
    subprocess.run([magick, str(src), "-resize", f"{px}x{px}", str(out)], check=True)
    print(f"  → {out.relative_to(ROOT)}  ({px}x{px})")


def gen_icons(chrome, magick, tmp):
    # the shipped, hand-readable SVG source
    (IMG / "favicon.svg").write_text(FAVICON_SVG.strip() + "\n")
    print(f"  → {(IMG / 'favicon.svg').relative_to(ROOT)}")

    tile = render_svg_master(chrome, tmp, FAVICON_SVG, "tile")       # transparent corners
    full = render_svg_master(chrome, tmp, FULLBLEED_SVG, "full")     # opaque, full-bleed
    mask = render_svg_master(chrome, tmp, MASKABLE_SVG, "mask")      # opaque, safe-zone glyph

    # favicon.ico = 16/32/48 from the rounded tile
    ico_parts = []
    for s in (16, 32, 48):
        p = tmp / f"ico{s}.png"
        subprocess.run([magick, str(tile), "-resize", f"{s}x{s}", str(p)], check=True)
        ico_parts.append(str(p))
    subprocess.run([magick, *ico_parts, str(IMG / "favicon.ico")], check=True)
    print(f"  → {(IMG / 'favicon.ico').relative_to(ROOT)}  (16/32/48)")

    resize(magick, tile, 192, IMG / "icon-192.png")
    resize(magick, tile, 512, IMG / "icon-512.png")
    resize(magick, full, 180, IMG / "apple-touch-icon.png")
    resize(magick, mask, 512, IMG / "icon-maskable-512.png")


def gen_og(chrome, magick, tmp):
    site = ROOT / "site"
    page = (site / "index.html").read_text()
    page = page.replace("<head>", f'<head><base href="{site.as_uri()}/">', 1).replace("</head>", OG_STYLE + "</head>", 1)
    page = re.sub(r"<script\b.*?</script>", "", page, flags=re.S)  # headless Chrome runs scripts regardless of flags
    html = tmp / "og.html"
    html.write_text(page)
    master = tmp / "og_2x.png"
    shoot(chrome, html.as_uri(), (1372, 720), str(master), scale=2, page=True)
    out = IMG / "og-image.png"
    subprocess.run([magick, str(master), "-resize", "1200x630!", "-strip", str(out)], check=True)
    print(f"  → {out.relative_to(ROOT)}  (1200x630)")


def main():
    which = sys.argv[1:] or ["icons", "og"]
    chrome, magick = find_chrome(), need_magick()
    IMG.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as td:
        tmp = Path(td)
        if "icons" in which:
            print("icons:")
            gen_icons(chrome, magick, tmp)
        if "og" in which:
            print("social card:")
            gen_og(chrome, magick, tmp)
    print("done.")


if __name__ == "__main__":
    main()
