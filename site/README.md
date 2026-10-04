# co:op website

The homepage and docs for co:op, served on GitHub Pages from this folder. Static
HTML/CSS/JS with no framework, no build step at serve time and no `node_modules`.
The look follows `brand/DESIGN.md`.

```
site/
  index.html            the homepage (generated: never edit it by hand)
  docs.html             the docs (hand-written, except its generated terminal windows)
  site.webmanifest      PWA manifest (name, theme color, app icons)
  robots.txt            allow-all + sitemap pointer
  sitemap.xml           the two pages, for crawlers
  assets/
    css/site.css        the design system, shared by both pages
    js/site.js          copy buttons, the agent picker, the homepage story, the scenes, the docs contents
    js/analytics.js     Mixpanel: page views and the copied setup commands
    fonts/              Inter (SIL Open Font License)
    img/                favicon + app icons + og-image (see "SEO assets")
```

## Preview locally

Any static server works:

```bash
cd site && python3 -m http.server 8000
# open http://localhost:8000
```

## The homepage and the docs' terminals

`tools/gen_site.py` writes `index.html` from `tools/site/index.tpl.html`. It
saves hand-typing what repeats: the icons, the provider marks (`tools/site/logos/`),
the terminal scenes and the commands you copy. It also fills the terminal windows
in `docs.html`: the docs are written by hand, except between each
`<!-- gen_site: NAME -->` and `<!-- /gen_site -->` marker, where the script writes
the scene `NAME`.

```bash
python3 tools/gen_site.py           # regenerate after editing the template or a scene
python3 tools/gen_site.py --check   # what make tools-test runs: fails if either page is stale
```

The terminal scenes copy the CLI's real output: the approved transcripts under
`internal/cli/testdata/approved/` and the Go code that prints each line, listed per
terminal in `TERMINAL_SOURCES`. When that output changes, `--check` fails, naming the
page, terminal and line, until the scene matches it again. The homepage's task-commit count comes from
`git log origin/main` when you regenerate.

Both pages load the stylesheet and scripts with `?v=` and a short hash of those files, which
`tools/gen_site.py` writes. Regenerate after changing them (`--check` fails until you do), and every
browser fetches the new copy instead of reusing a cached one.

## Deploy

`.github/workflows/pages.yml` uploads `site/` to GitHub Pages on every push to
`main` that touches it. One-time setup: **repo Settings → Pages → Source: "GitHub
Actions."** The workflow only publishes the committed files.

## SEO assets

The favicon, app icons and social card under `assets/img/` are generated,
committed artifacts. `tools/gen_seo_assets.py` draws the icons from the brand's
mark (`brand/assets/coop-flat.svg`) and renders the social card from the
homepage's own first screen, with headless Chrome and ImageMagick:

```bash
python3 tools/gen_seo_assets.py          # regenerate everything
python3 tools/gen_seo_assets.py icons    # just the favicon / app icons
python3 tools/gen_seo_assets.py og       # just the 1200×630 social card
```

Regenerate the card after changing the homepage's hero. Edit the canonical and
`og:url` base (and `site/CNAME`) if the site ever moves off `coop.dryga.com`.
