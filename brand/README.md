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
| [assets/coop-mark.svg](assets/coop-mark.svg) | Approved Contained entrance mark with transparent surround |
| [assets/coop-wordmark.svg](assets/coop-wordmark.svg) | Outlined co:op lettering; no font dependency |
| [assets/coop-lockup.svg](assets/coop-lockup.svg) | Complete logo for dark surfaces |
| [assets/coop-lockup-light.svg](assets/coop-lockup-light.svg) | Complete logo for light surfaces |
| [assets/coop-flat.svg](assets/coop-flat.svg) | Rounded graphite avatar/favicon tile |
| [../.github/assets/coop.png](../.github/assets/coop.png) | 512px GitHub/avatar PNG |
| [assets/approved-direction.png](assets/approved-direction.png) | Approved raster reference; not a production icon |
| [assets/fonts/InterVariable.woff2](assets/fonts/InterVariable.woff2) | Same unmodified variable font as Emisar |
| [assets/fonts/LICENSE.txt](assets/fonts/LICENSE.txt) | Inter's SIL Open Font License |

The production vectors reproduce the approved Contained entrance concept with flat fills.
The raster records the approval; its texture and option label are not part of the logo.
The former chicken illustration is replaced by the same mark used on the website.

## Implementation handoff

Use the supplied mark and outlined wordmark; preserve their geometry, negative space and
Coop colors. Supporting text stays in Inter. Keep the existing static website architecture,
content and URLs. The approved simplicity and palette also apply to icons, social cards,
README artwork and product cards in sibling repositories.

The site copies `coop-mark.svg` and `coop-wordmark.svg` into `site/assets/img/`;
`favicon.svg` is a copy of `coop-flat.svg`. Update both copies when changing the artwork.
Render PNG/ICO exports from those vectors, then refresh the website social cards and this
folder's desktop/mobile previews. See [the export instructions](../site/README.md#seo-assets).

The AndrewDryga project card uses a copy of `coop-mark.svg`. Ryker's public product card
and Protectorate's profile and Ryker design reference use copies of `coop-flat.svg`.
Those repositories keep their own assets; none loads from a sibling checkout at runtime.

## Font

The font is copied unchanged from Emisar's
`portal/apps/emisar_web/priv/static/fonts/InterVariable.woff2`. The upstream project is
[Inter](https://github.com/rsms/inter), and its
[license](https://github.com/rsms/inter/blob/master/LICENSE.txt) is included here. Nothing
at runtime needs an external font request or a path into a sibling repository.
