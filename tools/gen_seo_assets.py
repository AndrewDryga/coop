#!/usr/bin/env python3
"""
Generate the coop website's SEO / social assets (site/assets/img/*).

No third-party Python deps — the stdlib plus two CLIs already on the box:

  • headless Google Chrome   rasterizes SVG/HTML crisply (text, gradients, glows)
  • ImageMagick (`magick`)   downscales the master renders and bundles favicon.ico

Everything is generated from the source-of-truth strings in this file, so the mark
and the social card can never drift from each other. Re-run after changing a color,
the wordmark, or the tagline.

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
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
IMG = ROOT / "site" / "assets" / "img"

# --- brand tokens (mirror site/assets/css/site.css :root) --------------------
BG = "#0e1014"
WOOD = "#e6a95c"
CYAN = "#62d3f0"

# A wood gradient + the coop glyph (roof+body silhouette with a cyan loop-eye),
# authored on a 32x32 grid. Shared by every icon so they stay identical.
WOOD_GRAD = (
    '<linearGradient id="wood" x1="0" y1="0" x2="0" y2="1">'
    '<stop offset="0" stop-color="#f1b56a"/><stop offset="1" stop-color="#d0903f"/>'
    "</linearGradient>"
)
GLYPH = (
    '<path d="M16 4.4 L27 13.2 a1 1 0 0 1 0.4 0.8 L27.4 25 a2.2 2.2 0 0 1-2.2 2.2 '
    "L6.8 27.2 a2.2 2.2 0 0 1-2.2-2.2 L4.6 14 a1 1 0 0 1 0.4-0.8 Z\" fill=\"url(#wood)\"/>"
    '<circle cx="16" cy="18.6" r="4.7" fill="#0e1116"/>'
    '<circle cx="16" cy="18.6" r="4.7" fill="none" stroke="' + CYAN + '" stroke-width="3"/>'
)

# The shipped favicon: the mark on a dark rounded tile (transparent corners).
FAVICON_SVG = (
    '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" role="img" aria-label="coop">'
    "<defs>"
    '<linearGradient id="tile" x1="0" y1="0" x2="0" y2="1">'
    '<stop offset="0" stop-color="#1d232c"/><stop offset="1" stop-color="#0e1116"/></linearGradient>'
    + WOOD_GRAD
    + "</defs>"
    '<rect width="32" height="32" rx="7.2" fill="url(#tile)"/>'
    '<rect x="0.5" y="0.5" width="31" height="31" rx="6.7" fill="none" stroke="#ffffff" stroke-opacity="0.06"/>'
    + GLYPH
    + "</svg>"
)

# Full-bleed (no rounded corners) — the OS rounds it. Base for apple-touch.
FULLBLEED_SVG = (
    '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">'
    "<defs>"
    '<linearGradient id="bg" x1="0" y1="0" x2="0" y2="1">'
    '<stop offset="0" stop-color="#1d232c"/><stop offset="1" stop-color="#0e1116"/></linearGradient>'
    + WOOD_GRAD
    + "</defs>"
    '<rect width="32" height="32" fill="url(#bg)"/>'
    + GLYPH
    + "</svg>"
)

# Maskable — full-bleed bg, glyph scaled into the central safe zone (Android masks
# to a circle of ~80% diameter; keep content well inside it).
MASKABLE_SVG = (
    '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">'
    "<defs>"
    '<linearGradient id="bg" x1="0" y1="0" x2="0" y2="1">'
    '<stop offset="0" stop-color="#1d232c"/><stop offset="1" stop-color="#0e1116"/></linearGradient>'
    + WOOD_GRAD
    + "</defs>"
    '<rect width="32" height="32" fill="url(#bg)"/>'
    '<g transform="translate(16 16) scale(0.64) translate(-16 -16)">' + GLYPH + "</g>"
    "</svg>"
)

# The social card. {GLYPH} is injected so the inline logo matches the favicon.
OG_HTML = """<!doctype html>
<html lang="en"><head><meta charset="utf-8" />
<style>
  :root{
    --bg:#0e1014; --panel:#181c23; --border:#272d38; --border-soft:#20262f;
    --text:#ece7de; --muted:#9aa2af; --faint:#6b7280;
    --wood:#e6a95c; --cyan:#62d3f0; --green:#74c98a; --magenta:#c79be0;
    --sans: ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    --mono: ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, monospace;
  }
  *{margin:0;padding:0;box-sizing:border-box}
  html,body{width:1200px;height:630px}
  body{font-family:var(--sans); color:var(--text); background:var(--bg); position:relative; overflow:hidden;}
  .bg{position:absolute; inset:0;
    background:
      radial-gradient(900px 520px at 12% -8%, rgba(230,169,92,0.16), transparent 60%),
      radial-gradient(820px 520px at 108% 116%, rgba(98,211,240,0.14), transparent 58%),
      var(--bg);}
  .grid{position:absolute; inset:0; opacity:0.5;
    background-image:radial-gradient(rgba(255,255,255,0.045) 1px, transparent 1.4px); background-size:26px 26px;
    -webkit-mask-image:linear-gradient(180deg,#000 0%,transparent 78%); mask-image:linear-gradient(180deg,#000 0%,transparent 78%);}
  .wrap{position:relative; height:100%; padding:54px 60px 50px; display:flex; flex-direction:column;}
  .top{display:flex; align-items:center; gap:16px;}
  .mark{width:60px; height:60px; display:block; filter:drop-shadow(0 6px 16px rgba(0,0,0,.5));}
  .wordmark{font-weight:760; letter-spacing:-0.02em; font-size:38px;}
  .wordmark .colon{color:var(--wood);}
  .agents{margin-left:auto; display:flex; align-items:center; gap:12px; font-size:18px; color:var(--muted);
    font-weight:550; border:1px solid var(--border); background:rgba(24,28,35,.6); padding:9px 16px; border-radius:999px;}
  .agents b{color:var(--text); font-weight:650;} .agents .sep{color:var(--faint);}
  .main{flex:1; display:flex; align-items:center; gap:46px; margin-top:6px;}
  .left{flex:1.12; min-width:0;}
  .kicker{text-transform:uppercase; letter-spacing:0.16em; font-size:16px; font-weight:700; color:var(--wood); margin-bottom:18px;}
  h1{font-size:58px; line-height:1.07; font-weight:790; letter-spacing:-0.015em;}
  h1 .hl{color:var(--wood);}
  .lead{margin-top:22px; font-size:21px; line-height:1.5; color:#c4cbd4; max-width:30ch;}
  .term{flex:1; min-width:0; background:#0a0c10; border:1px solid var(--border); border-radius:14px;
    box-shadow:0 26px 60px -24px rgba(0,0,0,.85); overflow:hidden;}
  .term-bar{display:flex; align-items:center; gap:8px; padding:13px 16px;
    background:linear-gradient(180deg,#161a21,#12151b); border-bottom:1px solid var(--border-soft);}
  .dot{width:12px; height:12px; border-radius:50%;}
  .dot.r{background:#e5705f;} .dot.y{background:#e6b95c;} .dot.g{background:#6cc28a;}
  .term-title{margin-left:10px; font-family:var(--mono); font-size:15px; color:var(--faint);}
  .term-body{font-family:var(--mono); font-size:18px; line-height:1.95; padding:20px 22px; color:#d4d8df; white-space:nowrap;}
  .c-coop{color:var(--cyan); font-weight:700;} .c-dim{color:var(--faint);} .c-green{color:var(--green);}
  .c-wood{color:var(--wood);} .c-mag{color:var(--magenta);} .c-prompt{color:var(--green);}
  .foot{display:flex; align-items:center; gap:14px; color:var(--muted); font-size:18px;}
  .foot .url{font-family:var(--mono); color:var(--wood); font-weight:600;}
</style></head>
<body>
  <div class="bg"></div><div class="grid"></div>
  <div class="wrap">
    <div class="top">
      <svg class="mark" viewBox="0 0 32 32" xmlns="http://www.w3.org/2000/svg">
        <defs>
          <linearGradient id="tile" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#1d232c"/><stop offset="1" stop-color="#0e1116"/></linearGradient>
          <linearGradient id="wood" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#f1b56a"/><stop offset="1" stop-color="#d0903f"/></linearGradient>
        </defs>
        <rect width="32" height="32" rx="7.2" fill="url(#tile)"/>
        <rect x="0.5" y="0.5" width="31" height="31" rx="6.7" fill="none" stroke="#ffffff" stroke-opacity="0.06"/>
        {GLYPH}
      </svg>
      <span class="wordmark">co<span class="colon">:</span>op</span>
      <span class="agents"><b>Claude</b><span class="sep">&middot;</span><b>Codex</b><span class="sep">&middot;</span><b>Gemini</b></span>
    </div>
    <div class="main">
      <div class="left">
        <div class="kicker">Keep your agents busy while you sleep</div>
        <h1>Run agent loops <span class="hl">all&nbsp;night</span> &mdash; in a box they <span class="hl">can't&nbsp;escape</span>.</h1>
        <p class="lead">A disposable sandbox for coding agents you don't fully trust &mdash; secrets shadowed, the container as the cage.</p>
      </div>
      <div class="term">
        <div class="term-bar">
          <span class="dot r"></span><span class="dot y"></span><span class="dot g"></span>
          <span class="term-title">coop loop &mdash; drain the queue overnight</span>
        </div>
        <div class="term-body">
<span class="c-prompt">$</span> coop loop<br>
<span class="c-coop">coop:</span> box up &middot; secrets shadowed<br>
<span class="c-coop">coop:</span> <span class="c-dim">[1/7]</span> refactor auth  <span class="c-green">&check; done</span><br>
<span class="c-coop">coop:</span> <span class="c-dim">[2/7]</span> rate-limit tests  <span class="c-wood">&#10297;</span><br>
<span class="c-coop">coop:</span> <span class="c-mag">rode a rate limit</span> &middot; resuming<br>
        </div>
      </div>
    </div>
    <div class="foot">
      <span class="url">github.com/AndrewDryga/coop</span><span>&middot;</span>
      <span>one binary &middot; zero config &middot; your repo only</span>
    </div>
  </div>
</body></html>
"""


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


def shoot(chrome, url, px, out, *, scale=1):
    """Headless-screenshot `url` into a px*scale square (or WxH for the OG page)."""
    if isinstance(px, tuple):
        w, h = px
    else:
        w = h = px
    subprocess.run(
        [
            chrome, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-sandbox",
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
    (IMG / "favicon.svg").write_text(FAVICON_SVG + "\n")
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
    html = tmp / "og.html"
    html.write_text(OG_HTML.replace("{GLYPH}", GLYPH))
    master = tmp / "og_2x.png"
    shoot(chrome, html.as_uri(), (1200, 630), str(master), scale=2)  # 2400x1260 supersample
    out = IMG / "og-image.png"
    subprocess.run([magick, str(master), "-resize", "1200x630", str(out)], check=True)
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
