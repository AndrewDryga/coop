# co:op website identity

The handoff for the next co:op website: dark throughout, recognizably part of
Protectorate, and related to Emisar without becoming an amber version of it.
This directory is not published by the current GitHub Pages workflow.

Start with the [design guide](DESIGN.md), then open the
[visual reference](index.html) in a browser. The reference is a design specimen,
not a proposed replacement homepage or an actual product screen.

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

The SVG translates the approved silhouette into solid fills and real transparent
corners. The raster reference includes simulated lighting and a baked checkerboard;
neither is part of the production direction. The README chicken illustration is a
separate asset and stays unchanged.

## Implementation handoff

> Read `brand/DESIGN.md` and inspect `brand/index.html` before editing `site/`.
> Use the dark-only tokens and geometric website mark. Keep the README illustration
> unchanged. Preserve the existing static HTML/CSS/JS architecture, useful content
> and URLs. Build co:op's workspace-and-task story, not a recolored Emisar page.
> Verify the actual supported behavior before writing claims or terminal examples.
> Complete the guide's acceptance checklist before publication.

Font provenance: copied unchanged from Emisar's
`portal/apps/emisar_web/priv/static/fonts/InterVariable.woff2`; upstream project
[Inter](https://github.com/rsms/inter), with its
[license](https://github.com/rsms/inter/blob/master/LICENSE.txt) included here.
No external font request or sibling-repository path is needed at runtime.
