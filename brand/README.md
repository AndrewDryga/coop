# co:op website identity

This folder is the brand handoff the co:op website at coop.dryga.com was built from. The site is
dark throughout and recognizably part of Protectorate. It's related to Emisar without becoming an amber version
of it. The current GitHub Pages workflow doesn't publish this folder.

Start with the [design guide](DESIGN.md), then open the [visual reference](index.html) in a
browser. The reference is a design specimen. It isn't a proposed replacement homepage or a
real product screen.

## Files

| File | Purpose |
| --- | --- |
| [DESIGN.md](DESIGN.md) | Direction, family rules, components, page structure and acceptance checklist |
| [tokens.css](tokens.css) | Exact dark-theme values; not imported by the current website |
| [index.html](index.html) | Responsive, offline visual specimen |
| [preview.png](preview.png) | Desktop image of the reference |
| [preview-mobile.png](preview-mobile.png) | Mobile image of the reference |
| [specimen.css](specimen.css) | Specimen implementation, separate from the reusable tokens |
| [assets/coop-flat.svg](assets/coop-flat.svg) | Flat vector rendition of the approved geometric website mark |
| [assets/approved-direction.png](assets/approved-direction.png) | Approved raster reference; not a production icon |
| [assets/fonts/InterVariable.woff2](assets/fonts/InterVariable.woff2) | Same unmodified variable font as Emisar |
| [assets/fonts/LICENSE.txt](assets/fonts/LICENSE.txt) | Inter's SIL Open Font License |

The SVG turns the approved silhouette into solid fills with truly transparent corners. The
raster reference has simulated lighting and a baked-in checkerboard. Neither is part of the
production direction. The chicken illustration in the README is a separate asset, and it
stays unchanged.

## Implementation handoff

> Read `brand/DESIGN.md` and inspect `brand/index.html` before editing `site/`.
> Use the dark-only tokens and the geometric website mark. Keep the README illustration
> unchanged. Keep the existing static HTML/CSS/JS architecture, the useful content and the
> URLs. Build co:op's workspace-and-task story. Don't make a recolored Emisar page.
> Verify the product's real, supported behavior before you write claims or terminal examples.
> Complete the guide's acceptance checklist before publication.

## Font

The font is copied unchanged from Emisar's
`portal/apps/emisar_web/priv/static/fonts/InterVariable.woff2`. The upstream project is
[Inter](https://github.com/rsms/inter), and its
[license](https://github.com/rsms/inter/blob/master/LICENSE.txt) is included here. Nothing
at runtime needs an external font request or a path into a sibling repository.
