# co:op website design guide

Version 1 · September 2026. This guide set the direction for the website now at coop.dryga.com.
`site/assets/css/site.css` copies its token values under short names (`--bg`, `--amber`), and
`tools/gen_seo_assets.py` draws the icons from `assets/coop-flat.svg`. This guide, the
[tokens](tokens.css) and the [visual reference](index.html) form one handoff.

## 1. The direction

The site should look like a quiet, precise workspace for coding agents. That means
near-black surfaces, warm amber actions, a clear cyan boundary, generous typography and
useful evidence of work. It should feel capable and approachable, like a tool you can
understand and put to work. It shouldn't feel like a cyber-defense command center.

Everything is dark: marketing, documentation, search, navigation, code examples, menus,
forms, empty states and error pages. There are no light sections, no automatic light theme
and no theme switch. Respect forced-colors accessibility settings. Dark-only branding must
not turn off a user's assistive display preferences.

### The brief

| Field | Brief |
| --- | --- |
| Audience | Developers running coding agents on their own machines, and teams coordinating longer tasks |
| Explain | co:op brings sandboxing, task management and agent orchestration together |
| Buyer question | What can the agent reach, what will it work on, and what do I get to inspect when it finishes? |
| First useful action | Follow the installation and first-run instructions |
| Primary conversion | "Install co:op." Secondary: "Read the docs." Point them at an actual getting-started destination. Don't reuse Emisar's account-signup CTA. |
| Evidence | Real commands, the declared workspace, task state, changed code and verification results. Use current product behavior. Don't invent capability. |
| Constraints | The existing static site. Fast, accessible and readable without JavaScript. |

The chosen territory is a working boundary: a visible place where assigned work happens.
The other candidates were ruled out. A terminal-only treatment would hide task
orchestration. A green gate would copy Emisar. A chicken-themed website would turn the
README illustration into the whole brand.

## 2. One company, distinct products

Share the design grammar. Don't copy the page composition, and don't share every brand
asset.

| Element | Family connection to Emisar | co:op expression |
| --- | --- | --- |
| Typography | Self-hosted Inter; distinctive display cut; restrained mono | Same foundation; warm text and slightly denser working examples |
| Dark surfaces | Near-black canvas, quiet raised surfaces, thin light edges | Matte graphite; no glass or page-wide ambient glow |
| Layout | Generous section rhythm, strong headings, evidence near claims | Task → workspace → reviewable output |
| Controls | Compact rectangles, dark labels on solid primary buttons, visible focus | Amber primary action; cyan links and focus |
| Semantics | Success, warning and failure always have words/icons | Status colors separate from amber brand and cyan workspace meaning |
| Motion | Specific transitions, the same gentle easing, reduced-motion support | Short state changes; no gate pulse or perpetual activity animation |
| Brand device | A product-specific mechanism carries the story | The geometric coop and its circular entrance; bounded workspace diagrams |

Protectorate stays the umbrella brand. Its forcefield mark, orange accent and brand wordmark
are not co:op website components. Emisar's gate and emerald primary buttons belong to
Emisar. Ryker's logo belongs to Ryker. Don't redraw any sibling mark to match co:op. Using
the family typography for page content doesn't replace anyone's wordmark.

Put a quiet "A Protectorate product" endorsement in the footer, and a useful product family
section near the end of the page. Don't put three equal product pitches above co:op's first
CTA. Keep internal brand and design-guide links off the public site.

## 3. Logo and name

Write the product name as `co:op` in prose and navigation. The command, paths and
repository identifiers stay `coop`. Don't use "Coop OS" as a standalone category. Describe a
sandbox and orchestration system for coding agents. co:op isn't a replacement for macOS or
Linux.

Use [coop-flat.svg](assets/coop-flat.svg): the amber house, the cyan circular entrance and
the charcoal tile. It's a clean vector rendition of the
[approved visual direction](assets/approved-direction.png). It isn't a new symbol.

- Use only solid amber `#E6A95C`, cyan `#62D3F0` and charcoal `#111315`.
- Keep the roof silhouette, rounded corners, circular opening and centered tile.
- No bevel, wood texture, gradient, glow, extra outline or simulated lighting.
- Corners outside the tile use real transparency. Never fake it with checkerboard pixels.
- Default navigation: a 32px mark, a 12px gap and a 26px wordmark. Set the wordmark in Inter
  at 700 with tracking −0.03em. Only the colon is amber. Don't create another custom
  alphabet.
- Around a standalone tile, leave clear space of at least one quarter of its width. The nav
  lockup's explicit gap is the compact exception.
- Minimum tile size: 16px for favicon use and 24px in UI. In navigation, 32px is preferred.
  Check 16, 24, 32 and 64px at actual size before you export the production set.
- No enlarged decorative watermark of the logo behind the hero.

The README illustration in `.github/assets/coop.png` stays as it is. It isn't the favicon,
the navigation mark or the design system's primary visual device.

## 4. Color and dark material

Every exact value lives in [tokens.css](tokens.css). Don't scatter near-matching hex values
through components.

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

Most of the page is neutral. Amber is the first accent. Cyan is the second, used where its
purpose is visible. Don't stripe every heading, alternate colored cards, fill large areas
with either accent or use gradient text.

Amber branding doesn't mean a warning. A warning is a labeled status with a warning icon; an
amber surface alone isn't one. Cyan never means "safe", "approved" or "complete". The logo's
circular entrance is brand art. It makes no claim about a network connection.

### Surface construction

Build a surface from the surface fill, a 1px `white / 8%` ring and a subtle inset top
highlight. Default panels are matte and have no shadow. Use a soft shadow only for floating
menus and dialogs that must stand apart from the page, and make those surfaces opaque.

Borders between sections and rows are quiet neutral dividers. Save a colored frame for the
specifically labeled workspace boundary or a semantic state. Prose, headings and ordinary
feature explanations sit directly on the canvas. Don't put every paragraph in a card.

The quiet surface ring isn't enough for input boundaries. Inputs use
`--coop-control-border`, and their label stays visible. Primary amber buttons use
`--coop-on-accent` dark text, never white text. Essential copy needs at least 4.5:1
contrast. Large text and meaningful graphical boundaries need at least 3:1. Don't fade whole
controls with opacity, and that includes disabled controls.

## 5. Typography

Use the included, self-hosted InterVariable font with `font-display: swap`. It deliberately
shares Emisar's foundation. It isn't a generic font substitution: the display cut, tracking,
hierarchy and optical treatment are part of the family.

Headings use `font-optical-sizing: auto`, `font-feature-settings: "cv11" 1, "calt" 1, "liga" 1`
and balanced wrapping. Body copy keeps Inter's normal letterforms. Code uses the system mono
stack in the tokens. Never set whole paragraphs in monospace to make the page look
technical.

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

Use the fluid heading tokens to scale between the endpoints. Keep one H1 per page. Aim for
14–20ch hero headings, 25ch section titles and 60–68ch body columns. Avoid manual `<br>`
breaks that strand one word on mobile. Counts and changing values use tabular figures.
Terminal alignment uses mono. No tiny, low-contrast technical captions.

## 6. Layout and responsive behavior

Use a 4px spacing base: 4, 8, 12, 16, 24, 32, 48, 64, 96 and 128px. One wrapper owns each
gap. Don't combine a parent's gap and a child's margins for the same space.

- Maximum content width: 1200px. Gutters: 24px desktop, 20px mobile, 16px at 320px.
- Desktop sections: 96–128px vertical space. Mobile: 64px. Documentation: 48px between
  sections, 24px within them.
- Desktop navigation: 72px tall; mobile: 64px. When it's sticky, keep its background
  opaque, with a quiet bottom edge. Legibility must not depend on backdrop blur.
- Controls: 8px radius. Panels: 16px. Nested code insets: 8px with at least 8px of
  surrounding padding. Child corners must fit their parent concentrically.
- Buttons: 44px minimum height, 16–20px horizontal padding. Icon-only targets: at least
  44×44px. A larger hit area doesn't need a larger icon.
- Below 768px, narrative columns become a deliberate reading sequence. At 320px, actions
  wrap or stack. Don't squeeze a desktop diagram into miniature text.

### The signature composition: a working boundary

Put the hero statement above a broad, readable work artifact. Don't set it beside an
unrelated illustration. The bottom of the first viewport should already hint at the next
section. A useful diagram contains:

1. An assigned task, such as fixing a failing test.
2. A labeled workspace with the agent's allowed project and configured access.
3. Work progressing through named steps.
4. Reviewable changes and verification results.

Draw a thin cyan perimeter only around the workspace it describes. On desktop, show the task
and the output in adjacent columns with no box around them. The workspace is the one framed
artifact. On mobile, the diagram reads task → workspace → output from top to bottom, with
all labels intact. A meaningful boundary is continuous. Don't add decorative gaps, escaping
particles or beams that suggest uncontrolled access.

This is an explanatory diagram. It isn't an invented product dashboard, so label it as an
illustration. Show real screenshots and casts separately, and make them easy to tell apart
from illustrations. If you show a workflow animation, keep a complete static view too.

## 7. Components and behavior

| Component | Contract |
| --- | --- |
| Primary action | Amber fill, dark label, slightly lighter hover; one visually dominant CTA per decision area |
| Secondary action | Neutral surface/edge and normal text; no competing cyan fill |
| Inline link | Cyan; underline in body copy; visible focus; avoid vague "Learn more" |
| Focus | 2px cyan outline, 3px offset; never remove it; keep it unclipped |
| Code panel | Real selectable text, descriptive caption, neutral inset, working copy control |
| Copy state | Default → "Copied" → default; failure says "Select and copy the command"; announce through a polite live region |
| Task row | Plain-language task, status word/icon, optional result; mono only for ids/commands |
| Hover row | Neutral 4% white wash; do not recolor child text or turn a metric green |
| Status | Text plus a matching icon; only use "Passed" when verification actually passed |
| Docs navigation | Active item uses a neutral wash plus a clear marker and `aria-current`, not an amber success-like pill |
| Mobile menu | Labeled button, accurate `aria-expanded`, Escape closes and restores focus; overlay prevents accidental background interaction |
| Search | Labeled input; visible loading, results, no-results and error states; keyboard operation |

Use semantic HTML and native links and buttons. A disclosure is never a fake button on a
`<div>`. Icons share one consistent 24-unit grid and are typically 16–20px on screen. Hide
decorative icons from assistive technology. Keep the label when an icon alone would leave
people guessing.

### Motion

Use Emisar's easing, `cubic-bezier(0.16, 1, 0.3, 1)`, at co:op's quieter pace: 160ms for
hover, 200ms for state changes and at most 480ms for an optional first assembly.
Transitions name their properties. Never use `transition: all`.

No autoplay terminal typing, spinning logo, looping border light or scroll-jacking. An
optional workflow playback starts only when the user starts it. It needs pause and replay
controls, an equivalent transcript and a static initial state. Nothing starts hidden unless
an enhancement that initialized successfully can reveal it. Honor reduced motion in CSS and
JavaScript. Under reduced motion, turn off smooth scrolling and automatic playback as well.

## 8. Page structure and language

The homepage's job is to make the sandbox, the task system and orchestration feel like one
useful workflow. Use this sequence instead of a set of obligatory card grids:

1. What co:op does, why it helps and how to install it.
2. One understandable task, workspace and output illustration.
3. The boundary: what the selected configuration gives an agent access to, and what it
   doesn't. Link to the actual security model and its limits.
4. The work: how tasks are assigned, coordinated and checked. Show one useful command or a
   real task artifact beside the explanation.
5. The proof: a readable result, a genuine recording or an accurately labeled scripted
   walkthrough. Explain what it shows and what the example doesn't prove.
6. First run and practical questions: prerequisites, setup, supported environments, agent
   and provider requirements, configuration and recovery. Verify the details in the code
   and docs.
7. Product family, then the closing install CTA and a quiet footer.

Documentation uses the same palette and controls with less drama. It has a searchable
navigation column on wide screens, a readable article, and a compact contents disclosure on
mobile. Keep page URLs and deep links as they are. Code may scroll inside its own labeled
region, but the page must not scroll sideways. Explain a long command block before you show
it. Never crop essential output to make a screenshot look cleaner.

### Voice and claim boundaries

Write plainly. Name the agent, the task, the project folder, the access and the result. Say
"container-based sandbox" when the mechanism matters. Don't imply that co:op is an operating
system in the everyday sense.

This example shows the direction. It isn't required final copy:

> Give your agents room to work.
>
> Run coding agents in a sandbox on your computer. Give them tasks, coordinate
> the work, and review the changes.

The family explanation goes near the end:

> co:op gives coding agents a sandbox on your computer. Emisar extends controlled
> access to infrastructure and third-party tools. Ryker is the AI teammate built
> on top: you can work with it in Slack and GitHub.

Don't imply that installing co:op installs Emisar or Ryker, or that every integration is
automatic. Describe the setup the shipped products require.

Avoid "unbreakable", "can't escape", "zero risk", "secrets never enter", "enterprise-grade",
"done right" and made-up time-saving statistics. Don't show roadmap work as shipped. A claim
on the site isn't evidence just because it's already published. The site's terminal scenes are
reconstructions of real CLI output. Never label them as live or recorded customer results.

## 9. Implementation handoff and acceptance

Keep the existing hand-written HTML/CSS/JS site. This direction needs no new framework,
animation library, font CDN or build pipeline. When you implement it, consolidate the tokens
into the website's own stylesheet. Don't leave two competing runtime token systems. Keep
this guide as the design reference.

`tools/gen_seo_assets.py` generates the existing favicon, app-icon and OG exports. When you
implement the approved logo, update the script's canonical geometry and palette before you
regenerate the exports. An edit to favicon.svg alone gets overwritten. Produce favicon
16/32/48, Apple 180, PWA 192/512, maskable-safe 512 and OG 1200×630 assets from the same
source. Don't bake preview checkerboards into them. The production asset rollout is
separate from this guide.

### Before calling the new website done

- [ ] All site surfaces stay dark, even when the OS asks for light mode.
- [ ] Side by side, Emisar and co:op look related. With their logos covered, you can still
  tell them apart: shared type and craft, different color, device and composition.
- [ ] The README illustration is unchanged, and the website mark matches the approved
  geometry.
- [ ] A visitor can identify the product, the next step and the useful mechanism in five
  seconds.
- [ ] Claims and commands match current product behavior, and examples state their
  provenance.
- [ ] You've rendered and inspected desktop 1440×1000, constrained 1024×900, mobile
  390×844, small 320px and short 1440×700 layouts, including the footer and the docs.
- [ ] No clipped controls, sideways page scrolling or lost content at 200% zoom.
- [ ] Keyboard navigation, skip link, mobile menu, search and copy failure paths work.
- [ ] Text contrast is checked against the actual backgrounds. Focus, control and diagram
  boundaries stay visible. Color alone never carries a status.
- [ ] The site is usable with reduced motion, with no JavaScript, with forced colors and
  with the font unavailable.
- [ ] The font and its license are self-hosted. Image dimensions prevent layout shift.
- [ ] Images use efficient formats, diagrams use SVG or HTML, and playback assets load on
  demand.
- [ ] Semantic HTML, one H1, useful metadata, existing canonical URLs, sitemap, social cards
  and docs deep links remain correct.
- [ ] The rendered design review and the repository's relevant checks pass on the final
  tree.

## 10. Source notes

This guide is grounded in the repositories as inspected on 2026-09-14. The sources below are
authoring references. They aren't runtime dependencies, and they aren't instructions to
import Phoenix components.

- Emisar: `portal/.agent/kb/rules/design-system.md`; the actual font, focus, motion and
  surface recipes in `portal/apps/emisar_web/assets/css/app.css`; the heading, button and
  nav components in `portal/apps/emisar_web/lib/emisar_web/components/marketing_components.ex`;
  the narrative in `controllers/marketing_html/home.html.heex` in that same app.
- co:op: `site/assets/css/site.css`, `site/assets/js/site.js`, `site/index.html`,
  `site/docs.html`, `site/README.md` and `tools/gen_seo_assets.py`.
- Protectorate: `protectorate/brand/README.md` and `protectorate/brand/DESIGN.md` in the
  sibling repository, for the umbrella identity. They aren't the authority on website fonts.

The creative-direction skill shaped the product-specific concept and hierarchy. The
interface-polish skill shaped typography, dark surfaces, focus and motion. The specimen
demonstrates those rules. It isn't finished website content.
