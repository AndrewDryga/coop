# co:op sandbox-first website mockup

Local, static prototype. This is not the live site or a new runtime release.
Open `index.html` directly, or serve the repository root and visit
`/brand/website/`. No package installation or build is required.

## Direction

The user’s latest brief replaces the workflow-first pitch: lead with a sandbox
and hidden sensitive files, then supported agents; orchestration is secondary.
The existing approved logo, dark palette, and Inter display cut are retained.
The public site, README illustration, and original brand specimen are unchanged.

Three creative territories were considered:

| Territory | Narrative and visual device | Judgment |
| --- | --- | --- |
| Working boundary | Oversized editorial headline, project files mapped to the agent’s view, explicit exclusions; matte graphite, amber actions, cyan boundary; user-controlled file example | Chosen: strongest clarity and product fit; simple static implementation; accessible without motion. Main risk is looking like an actual app UI, addressed by the illustration label. |
| Private by pattern | Oversized filenames and exclusion rules become the page’s graphic language; same type and palette; minimal state changes; proof from rule examples | Memorable and inexpensive, but risks making co:op feel like a secret scanner. |
| The workbench | A project environment, its tools and agents form the hero; dense type-led composition and restrained interaction; proof from commands | Good breadth but weaker security-first hierarchy, more visual complexity, greater mobile cost. |

The creative-direction and content skills drove the outcome/mechanism hierarchy,
ordinary-language copy, explicit example provenance, and restrained composition.
The approved family font takes precedence over the general design skill’s default
advice against Inter. The pattern is product-specific rather than a generic SaaS grid.

## Claim boundaries

- Diagram is an interactive explanation, not a real co:op GUI or live security test.
- Secret hiding covers known sensitive filenames and explicitly excluded paths.
  It is not universal content detection, Git-history scrubbing, or a guarantee that
  all provider credentials remain outside the sandbox.
- No universal microVM, deny-by-default network, zero-leakage, or benchmark claim.
- Normal runs edit the actual checkout; forks are optional.
- Local execution does not mean local inference or no communication with providers.
- Core claims were checked against README.md, runtime/runtime.go, box/secrets.go,
  and the v8.1.0 source in the preceding research. New filtered networking is not
  marketed as a released capability.
- No signup, analytics, install execution, customer evidence, or automatic requests.
  Agent buttons change a displayed command only. Setup links open the existing docs.
- Prototype is noindex. Publishing requires a separate request and release-matched
  installation, canonical, metadata, social-card and sitemap checks.

## Assets

- co:op mark and licensed Inter font: existing `../assets/` files, unmodified.
- Emisar: copied from `emisar/portal/apps/emisar_web/priv/static/images/brand/emisar-icon.svg`.
- Ryker: copied from `protectorate/brand/ryker/mark-mint.svg`.
- No external fonts or runtime dependence on sibling checkouts.

## Previews

Rendered desktop, mobile, and first-viewport PNGs sit next to the HTML. The file
access and agent selectors, FAQ, navigation, narrow layouts, reduced motion, and
no-JavaScript presentation are checked before handoff. These checks validate the
website prototype only; they are not sandbox/runtime qualification.

Verified in headless Chrome at 1440, 1024, 720, 580, 390, and 320 CSS pixels:
no horizontal overflow, loaded images/font, one H1, and valid in-page anchors.
File examples, agent selection, native FAQ, and keyboard activation passed.
Reduced-motion, forced-colors layout, and essential no-JavaScript content passed.
Measured text contrast: primary action 9.04:1, hidden-file labels 6.91:1,
illustration caption 7.10:1. These samples are not a full accessibility audit.
