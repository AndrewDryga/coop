# co:op website identity

This folder is the brand handoff the co:op website at coop.dryga.com was built from. The site is
dark throughout and recognizably part of Protectorate. It's related to Emisar without becoming an
amber version of it. The GitHub Pages workflow doesn't publish this folder.

Start with the [design guide](DESIGN.md).

## Files

| File | Purpose |
| --- | --- |
| [DESIGN.md](DESIGN.md) | Direction, family rules, components, page structure and acceptance checklist |
| [assets/coop-mark.svg](assets/coop-mark.svg) | Approved Contained entrance mark with transparent surround |
| [assets/coop-wordmark.svg](assets/coop-wordmark.svg) | Outlined co:op lettering, with no font dependency |
| [assets/coop-lockup.svg](assets/coop-lockup.svg) | Complete logo for dark surfaces |
| [assets/coop-lockup-light.svg](assets/coop-lockup-light.svg) | Complete logo for light surfaces |
| [assets/coop-flat.svg](assets/coop-flat.svg) | Rounded graphite avatar/favicon tile |
| [../.github/assets/coop.png](../.github/assets/coop.png) | 512px GitHub/avatar PNG |

The production vectors reproduce the approved Contained entrance concept with flat fills.
The README uses the same mark as the website.

## Implementation handoff

Use the supplied mark and outlined wordmark, and preserve their geometry, negative space and
co:op colors. Supporting text stays in Inter. Keep the existing static website architecture,
content and URLs. The approved simplicity and palette also apply to icons, social cards,
README artwork and product cards in sibling repositories.

The site copies `coop-mark.svg` and `coop-wordmark.svg` into `site/assets/img/`, and
`favicon.svg` is a copy of `coop-flat.svg`. Update both copies when you change the artwork.
Render PNG/ICO exports from those vectors, then refresh the website social cards. See
[the export instructions](../site/README.md#seo-assets).

The AndrewDryga project card uses a copy of `coop-mark.svg`. Ryker's public product card
and Protectorate's profile and Ryker design reference use copies of `coop-flat.svg`.
Those repositories keep their own assets, and none loads from a sibling checkout at runtime.

## Font

The site's copy in `site/assets/fonts/` is unchanged from Emisar's
`portal/apps/emisar_web/priv/static/fonts/InterVariable.woff2`. The upstream project is
[Inter](https://github.com/rsms/inter), and its
[license](https://github.com/rsms/inter/blob/master/LICENSE.txt) sits beside it. Nothing
at runtime needs an external font request or a path into a sibling repository.
