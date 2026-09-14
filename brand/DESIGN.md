# co:op — website design guide

Version 1 · September 2026 · Direction for the next website, not a claim that it
is already implemented. This guide, the [tokens](tokens.css), and the
[visual reference](index.html) form one handoff.

## 1. The direction

A quiet, precise workspace for coding agents. Near-black surfaces, warm amber
actions, a clear cyan boundary, generous typography, and useful evidence of work.
The feeling is capable and approachable: a tool you can understand and put to work,
not a cyber-defense command center.

Everything is dark: marketing, documentation, search, navigation, code examples,
menus, forms, empty states and error pages. No light sections, automatic light
theme or theme switch. Respect forced-colors accessibility settings; dark-only
branding must not disable a user's assistive display preferences.

### The brief

- Audience: developers running coding agents on their own machines and teams
  coordinating longer tasks.
- Explain: co:op brings sandboxing, task management and agent orchestration together.
- Buyer question: what can the agent reach, what will it work on, and what do I get
  to inspect when it finishes?
- First useful action: follow the installation and first-run instructions.
- Primary conversion: “Install co:op.” Secondary: “Read the docs.” Use an actual
  getting-started destination, not Emisar's account-signup CTA.
- Evidence: real commands, the declared workspace, task state, changed code and
  verification results. Use current product behavior, not invented capability.
- Constraints: existing static site; fast, accessible, readable without JavaScript.

The chosen territory is a working boundary: a visible place where assigned work
happens. A terminal-only treatment would hide task orchestration. A green gate
would copy Emisar. A chicken-themed website would turn the README illustration
into the whole brand. None is the direction.

## 2. One company, distinct products

Share the design grammar, not the page composition or every brand asset.

| Element | Family connection to Emisar | co:op expression |
| --- | --- | --- |
| Typography | Self-hosted Inter; distinctive display cut; restrained mono | Same foundation; warm text and slightly denser working examples |
| Dark surfaces | Near-black canvas, quiet raised surfaces, thin light edges | Matte graphite; no glass or page-wide ambient glow |
| Layout | Generous section rhythm, strong headings, evidence near claims | Task → workspace → reviewable output |
| Controls | Compact rectangles, dark labels on solid primary buttons, visible focus | Amber primary action; cyan links and focus |
| Semantics | Success, warning and failure always have words/icons | Status colors separate from amber brand and cyan workspace meaning |
| Motion | Specific transitions, the same gentle easing, reduced-motion support | Short state changes; no gate pulse or perpetual activity animation |
| Brand device | A product-specific mechanism carries the story | The geometric coop and its circular entrance; bounded workspace diagrams |

Protectorate remains the umbrella brand. Its forcefield mark, orange accent and
brand wordmark are not co:op website components. Emisar's gate and emerald primary
buttons remain Emisar's. Ryker's logo remains Ryker's. Do not redraw any sibling
mark to match co:op; family typography for page content does not replace wordmarks.

Use a quiet “A Protectorate product” endorsement in the footer and a useful product
family section near the end of the page. Do not put three equal product pitches
above co:op's first CTA. Keep internal brand/design-guide links off the public site.

## 3. Logo and name

Write the product name `co:op` in prose and navigation. The command, paths and
repository identifiers remain `coop`. Avoid “Coop OS” as a standalone category:
describe a sandbox and orchestration system for coding agents, not a replacement
for macOS or Linux.

Use [coop-flat.svg](assets/coop-flat.svg): the amber house, cyan circular entrance
and charcoal tile. This is a clean vector rendition of the
[approved visual direction](assets/approved-direction.png), not a new symbol.

- Solid amber `#E6A95C`, cyan `#62D3F0` and charcoal `#111315` only.
- Keep the roof silhouette, rounded corners, circular opening and centered tile.
- No bevel, wood texture, gradient, glow, extra outline or simulated lighting.
- Corners outside the tile use genuine transparency, never checkerboard pixels.
- Default navigation: 32px mark, 12px gap, 26px wordmark. Use Inter at 700,
  tracking −0.03em; only the colon is amber. Do not create another custom alphabet.
- Clear space around a standalone tile: at least one quarter of its width.
  The nav lockup's explicit gap is the compact exception.
- Minimum tile size: 16px for favicon use; 24px in UI; 32px preferred in navigation.
  Check 16, 24, 32 and 64px at actual size before exporting the production set.
- No enlarged decorative watermark of the logo behind the hero.

The existing README illustration in `.github/assets/coop.png` stays as it is.
It is not the favicon, navigation mark, or design system's primary visual device.

## 4. Color and dark material

All exact values live in [tokens.css](tokens.css). Do not scatter near-matching
hex values through components.

| Token | Value | Use |
| --- | --- | --- |
| `--coop-bg` | `#0E1014` | All page canvas |
| `--coop-surface` | `#15191F` | Earned panels: code, diagram, search results |
| `--coop-raised` | `#1C222B` | Menus, dialogs, small lifted controls |
| `--coop-inset` | `#0A0C10` | Recessed terminal/code inside a panel |
| `--coop-text` | `#ECE7DE` | Headings and primary labels |
| `--coop-body` | `#BEC5CE` | Running copy |
| `--coop-muted` | `#9AA2AF` | Essential secondary labels and captions |
| `--coop-amber` | `#E6A95C` | Primary action, wordmark colon, sparse emphasis |
| `--coop-cyan` | `#62D3F0` | Links, focus, labeled workspace boundary |
| `--coop-success` | `#36E6A5` | Verified success/allowed/healthy, with label |
| `--coop-warning` | `#FCD34D` | Needs attention, with warning icon and label |
| `--coop-danger` | `#FDA4AF` | Failed/denied/destructive state, with label |

Most of the page is neutral. Amber is the first accent; cyan is the second, used
where its purpose is visible. Do not stripe every heading, alternate colored cards,
fill large areas with either accent, or use gradient text.

Amber branding is not a warning. A warning is a labeled status with a warning icon,
not simply an amber surface. Cyan never means “safe,” “approved” or “complete.”
The logo's circular entrance is brand art, not a claim about a network connection.

### Surface construction

Use the surface fill, a 1px `white / 8%` ring and a subtle inset top highlight.
Default panels are matte and shadowless. Use a soft shadow only for floating
menus/dialogs that must separate from the page; make those surfaces opaque.

Borders between sections and rows are quiet neutral dividers. A colored frame is
reserved for the specifically labeled workspace boundary or a semantic state.
Prose, headings and ordinary feature explanations sit directly on the canvas;
do not put every paragraph in a card.

The quiet surface ring is not sufficient for input boundaries. Inputs use
`--coop-control-border`; keep their label visible. Primary amber buttons use
`--coop-on-accent` dark text, never white text. Essential copy must meet at least
4.5:1 contrast; large text and meaningful graphical boundaries at least 3:1.
Do not fade entire controls with opacity, including disabled controls.

## 5. Typography

Use the included, self-hosted InterVariable font with `font-display: swap`.
This deliberately shares Emisar's foundation. It is not a generic font substitution:
the display cut, tracking, hierarchy and optical treatment are part of the family.

Headings use `font-optical-sizing: auto`, `font-feature-settings: "cv11" 1,
"calt" 1, "liga" 1`, and balanced wrapping. Body copy keeps Inter's normal
letterforms. Code uses the system mono stack in the tokens. Never set whole
paragraphs in monospace to make the page look technical.

| Role | Desktop | Mobile | Weight / line height / tracking |
| --- | --- | --- | --- |
| Hero | 72px | 40px | 700 / 1.05 / −0.035em |
| Section title | 48px | 32px | 650 / 1.12 / −0.03em |
| Subsection | 24px | 22px | 600 / 1.25 / −0.02em |
| Lead | 20px | 18px | 400 / 1.6 / normal |
| Body | 16px | 16px | 400 / 1.7 / normal |
| Control / supporting text | 14px | 14px | 500 / 1.5 / normal |
| Eyebrow | 12px | 12px | 600 / 1.4 / 0.08em; uppercase |
| Terminal / code | 14px | 14px | 400 / 1.7 / normal |

Use fluid heading tokens between the endpoints. One H1 per page. Aim for 14–20ch
hero headings, 25ch section titles and 60–68ch body columns. Avoid manual `<br>`
breaks that strand one word on mobile. Counts and changing values use tabular
figures; terminal alignment uses mono. No tiny low-contrast technical captions.

## 6. Layout and responsive behavior

Use a 4px spacing base: 4, 8, 12, 16, 24, 32, 48, 64, 96 and 128px.
One wrapper owns each gap. Do not combine parent gap and child margins for the
same space.

- Maximum content width: 1200px. Gutters: 24px desktop, 20px mobile, 16px at 320px.
- Desktop sections: 96–128px vertical space. Mobile: 64px. Documentation: 48px
  between sections, 24px within them.
- Desktop navigation: 72px tall; mobile: 64px. Keep its background opaque when
  sticky, with a quiet bottom edge. Do not require backdrop blur for legibility.
- Controls: 8px radius. Panels: 16px. Nested code insets: 8px with at least 8px
  of surrounding padding. Child corners must fit their parent concentrically.
- Buttons: 44px minimum height, 16–20px horizontal padding. Icon-only targets:
  at least 44×44px. Larger hit areas do not require larger icons.
- Below 768px, narrative columns become a deliberate reading sequence. At 320px,
  actions wrap or stack. Do not squeeze a desktop diagram into miniature text.

### The signature composition: a working boundary

Place the hero statement above a broad, readable work artifact, not beside an
unrelated illustration. The next section should already be suggested at the bottom
of the first viewport. A useful diagram contains:

1. An assigned task, such as fixing a failing test.
2. A labeled workspace with the agent's allowed project and configured access.
3. Work progressing through named steps.
4. Reviewable changes and verification results.

Use a thin cyan perimeter only around the workspace it describes. On desktop,
show the task and output in adjacent unboxed columns; the workspace is the one
framed artifact. On mobile, read task → workspace → output vertically with all
labels intact. A meaningful boundary is continuous; don't add decorative gaps,
escaping particles, or beams suggesting uncontrolled access.

This is an explanatory diagram, not an invented product dashboard. Label it as an
illustration. Show real screenshots/casts separately and distinguish them from
illustrations. If showing a workflow animation, preserve a complete static view.

## 7. Components and behavior

| Component | Contract |
| --- | --- |
| Primary action | Amber fill, dark label, slightly lighter hover; one visually dominant CTA per decision area |
| Secondary action | Neutral surface/edge and normal text; no competing cyan fill |
| Inline link | Cyan; underline in body copy; visible focus; avoid vague “Learn more” |
| Focus | 2px cyan outline, 3px offset; never remove it; keep it unclipped |
| Code panel | Real selectable text, descriptive caption, neutral inset, working copy control |
| Copy state | Default → “Copied” → default; failure says “Select and copy the command”; announce through a polite live region |
| Task row | Plain-language task, status word/icon, optional result; mono only for ids/commands |
| Hover row | Neutral 4% white wash; do not recolor child text or turn a metric green |
| Status | Text plus a matching icon; only use “Passed” when verification actually passed |
| Docs navigation | Active item uses a neutral wash plus a clear marker and `aria-current`, not an amber success-like pill |
| Mobile menu | Labeled button, accurate `aria-expanded`, Escape closes and restores focus; overlay prevents accidental background interaction |
| Search | Labeled input; visible loading, results, no-results and error states; keyboard operation |

Use semantic HTML and native links/buttons. A disclosure is not a fake button on
a `<div>`. Icons share one consistent 24-unit grid, typically 16–20px on screen;
decorative icons are hidden from assistive technology. Keep labels when an icon
would require guessing.

### Motion

Use Emisar's easing, `cubic-bezier(0.16, 1, 0.3, 1)`, with co:op's quieter pace:
160ms hover, 200ms state changes, at most 480ms for an optional first assembly.
Transitions name properties; never use `transition: all`.

No autoplay terminal typing, spinning logo, looping border light or scroll-jacking.
An optional workflow playback must be user initiated with pause/replay controls,
an equivalent transcript, and a static initial state. No content starts hidden
unless a successfully initialized enhancement can reveal it. Honor reduced motion
in CSS and JavaScript; disable smooth scrolling and automatic playback as well.

## 8. Page structure and language

The homepage's job is to make the sandbox, task system and orchestration feel like
one useful workflow. Use this sequence, not an obligatory collection of card grids:

1. What co:op does, why it helps, and how to install it.
2. One understandable task/workspace/output illustration.
3. The boundary: what the selected configuration gives an agent access to, and
   what it does not. Link to the actual security model and its limits.
4. The work: how tasks are assigned, coordinated and checked. Show one useful
   command or real task artifact beside the explanation.
5. The proof: a readable result, genuine recording or accurately labeled scripted
   walkthrough. Explain what is being shown and what the example does not prove.
6. First run and practical questions: prerequisites, setup, supported environments,
   agent/provider requirements, configuration and recovery. Verify details in code/docs.
7. Product family, then the closing install CTA and quiet footer.

Documentation uses the same palette and controls but less drama: a searchable
navigation column on wide screens, a readable article and a compact contents
disclosure on mobile. Preserve page URLs and deep links. Code may scroll within its
own labeled region; the page must not scroll sideways. Explain before showing a
long command block. Never crop essential output to make a screenshot look cleaner.

### Voice and claim boundaries

Write plainly. Name the agent, task, project folder, access and result. Say
“container-based sandbox” when the mechanism matters; do not imply that co:op is
an operating system in the everyday sense.

Example direction, not mandatory final copy:

> Give your agents room to work.
>
> Run coding agents in a sandbox on your computer. Give them tasks, coordinate
> the work, and review the changes.

Family explanation, near the end:

> co:op gives coding agents a sandbox on your computer. Emisar extends controlled
> access to infrastructure and third-party tools. Ryker is the AI teammate built
> on top: you can work with it in Slack and GitHub.

Do not imply that installing co:op installs Emisar/Ryker, or that every integration
is automatic. Describe the setup the shipped products actually require.

Avoid “unbreakable,” “can't escape,” “zero risk,” “secrets never enter,”
“enterprise-grade,” “done right,” and made-up time-saving statistics. Do not show
roadmap work as shipped. Existing site claims are not evidence just because they
are already published. Existing casts are scripted reconstructions; a new design
must not relabel them as live or recorded customer results.

## 9. Implementation handoff and acceptance

Keep the existing hand-written HTML/CSS/JS site. No new framework, animation
library, font CDN or build pipeline is needed for this direction. Consolidate the
tokens into the website's owned stylesheet during implementation; do not leave two
competing runtime token systems. Keep the guide as the design reference.

The existing favicon/app-icon/OG exports are generated by
`tools/gen_seo_assets.py`. When implementing the approved logo, update its canonical
geometry and palette before regenerating exports; editing only favicon.svg will
be overwritten. Produce favicon 16/32/48, Apple 180, PWA 192/512, maskable-safe
512 and OG 1200×630 assets from the same source. Do not bake preview checkerboards
into them. The production asset rollout is separate from this guide.

### Before calling the new website done

- [ ] All site surfaces remain dark, even when the OS requests light mode.
- [ ] Emisar and co:op look related side by side but remain distinguishable with
  their logos covered: shared type/craft, different color/device/composition.
- [ ] README illustration unchanged; website mark matches the approved geometry.
- [ ] A visitor can identify the product, next step and useful mechanism in five seconds.
- [ ] Claims and commands match current product behavior; examples state their provenance.
- [ ] Desktop 1440×1000, constrained 1024×900, mobile 390×844, small 320px and
  short 1440×700 layouts have been rendered and inspected, including footer/docs.
- [ ] No clipped controls, sideways page scrolling or lost content at 200% zoom.
- [ ] Keyboard navigation, skip link, mobile menu, search and copy failure paths work.
- [ ] Text contrast is checked against actual backgrounds; focus/control/diagram
  boundaries remain visible; color alone never carries a status.
- [ ] Reduced motion, no JavaScript, forced colors and unavailable font are usable.
- [ ] The font and its license are self-hosted; image dimensions prevent layout shift.
- [ ] Images use efficient formats; diagrams use SVG/HTML; playback assets load on demand.
- [ ] Semantic HTML, one H1, useful metadata, existing canonical URLs, sitemap,
  social cards and docs deep links remain correct.
- [ ] Rendered design review and the repository's relevant checks pass on the final tree.

## 10. Source notes

Grounded in the repositories as inspected on 2026-09-14. These are authoring
references, not runtime dependencies or instructions to import Phoenix components.

- Emisar: `portal/.agent/kb/rules/design-system.md`; actual font, focus, motion and
  surface recipes in `portal/apps/emisar_web/assets/css/app.css`; heading/button/nav
  components in `portal/apps/emisar_web/lib/emisar_web/components/marketing_components.ex`;
  narrative in `controllers/marketing_html/home.html.heex` in that same app.
- co:op: `site/assets/css/site.css`, `site/assets/js/site.js`, `site/index.html`,
  `site/docs.html`, `site/README.md` and `tools/gen_seo_assets.py`.
- Protectorate: sibling repository `protectorate/brand/README.md` and
  `protectorate/brand/DESIGN.md` for umbrella identity, not website font authority.

The creative-direction skill informed the product-specific concept and hierarchy;
the interface-polish skill informed typography, dark surfaces, focus and motion.
The specimen demonstrates those rules, not finished website content.
