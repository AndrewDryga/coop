import html
import re
import subprocess
import sys
import unittest
from html.parser import HTMLParser
from pathlib import Path


SITE = Path(__file__).resolve().parents[1] / "site"
sys.path.insert(0, str(SITE.parent / "tools"))
import gen_site  # noqa: E402 — the generator owns the terminals and their sources


class Page(HTMLParser):
    def __init__(self, path):
        super().__init__()
        self.elements = []
        self.copy_sources = []  # the text of each command a Copy button copies
        self.main_nav = []  # the links in the main navigation
        self.text = []
        self.copying = False
        self.in_main_nav = False
        self.feed(path.read_text(encoding="utf-8"))

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        self.elements.append((tag, attrs))
        if tag == "code" and "data-copy-source" in attrs:
            self.copying = True
            self.copy_sources.append("")
        if tag == "nav" and attrs.get("aria-label") == "Main":
            self.in_main_nav = True
        if tag == "a" and self.in_main_nav:
            self.main_nav.append(attrs.get("href"))

    def handle_endtag(self, tag):
        if tag == "code":
            self.copying = False
        if tag == "nav":
            self.in_main_nav = False

    def handle_data(self, data):
        self.text.append(data)
        if self.copying:
            self.copy_sources[-1] += data


class SiteContentTest(unittest.TestCase):
    def test_evaluations_are_navigable_and_copyable(self):
        page = Page(SITE / "docs.html")
        self.assertTrue(any(tag == "a" and attrs.get("href") == "#evals" for tag, attrs in page.elements))
        self.assertEqual(sum(tag == "section" and attrs.get("id") == "evals" for tag, attrs in page.elements), 1)
        source = (SITE / "docs.html").read_text(encoding="utf-8")
        section = source.split('id="evals">', 1)[1].split("</section>", 1)[0]
        commands = [html.unescape(re.sub(r"<[^>]+>", "", c)) for c in re.findall(r"<code data-copy-source>(.*?)</code>", section, re.S)]
        self.assertGreaterEqual(len(commands), 3)
        self.assertIn("--dry-run", commands[0])

    def test_docs_commands_copy_as_runnable_shell(self):
        source = (SITE / "docs.html").read_text(encoding="utf-8")
        # every shell command sits in a copyable list, never in a plain code block, so each one copies alone
        for block in re.findall(r'<pre class="code"[^>]*>(.*?)</pre>', source, re.S):
            self.assertNotIn('<span class="k">', block, "a shell command in a plain code block has no Copy button of its own")
        commands = [html.unescape(re.sub(r"<[^>]+>", "", c)) for c in re.findall(r"<code data-copy-source>(.*?)</code>", source, re.S)]
        self.assertGreater(len(commands), 30)
        # a note sits beside its command in one line down to a 1024px window: 57 characters for both
        for block in re.findall(r'<ul class="cmds"[^>]*>(.*?)</ul>', source, re.S):
            rows = re.findall(r'<code data-copy-source>(.*?)</code>(?:<span class="cmd-note">(.*?)</span>)?', block, re.S)
            widest = max(len(html.unescape(re.sub(r"<[^>]+>", "", code))) for code, _ in rows)
            for code, note in filter(lambda row: row[1], rows):
                with self.subTest(note=note):
                    self.assertLessEqual(widest + len(html.unescape(note)), 57, "this note wraps beside its block's widest command")
        for command in commands:
            with self.subTest(command=command):
                self.assertFalse(command.startswith("$") or "#" in command, "a copied command carries a prompt or a comment")
                runnable = re.sub(r"<[\w-]+>", "placeholder", command)  # <host>, <url>: names you fill in
                parsed = subprocess.run(["sh", "-n"], input=runnable, text=True, capture_output=True, check=False)
                self.assertEqual(parsed.returncode, 0, parsed.stderr)

    def test_service_examples_use_the_supported_delete_flag(self):
        for path in (SITE.parent / "README.md", SITE / "docs.html"):
            with self.subTest(page=path.name):
                text = "".join(Page(path).text) if path.suffix == ".html" else path.read_text(encoding="utf-8")
                self.assertEqual(re.findall(r"\bcoop down (?:-v|--volumes)\b", text), [])
                self.assertTrue("coop down --delete-volumes" in text, "missing supported volume-deletion command")

    def test_setup_commands_copy_as_runnable_shell(self):
        page = Page(SITE / "index.html")
        buttons = [attrs for tag, attrs in page.elements if tag == "button" and "data-copy" in attrs]
        # what site.js copies: the command's text without its "$ " prompt
        commands = [re.sub(r"^\$\s*", "", text).strip() for text in page.copy_sources]
        self.assertEqual(len(buttons), len(commands))
        self.assertEqual(commands, [
            "curl -fsSL https://coop.dryga.com/install.sh | sh",
            "coop login claude",
            "cd your-project && coop init",
            "coop claude",
        ])
        for command in commands:
            with self.subTest(command=command):
                parsed = subprocess.run(["sh", "-n"], input=command, text=True, capture_output=True, check=False)
                self.assertEqual(parsed.returncode, 0, parsed.stderr)

    def test_case_figures_sit_outside_their_case_block(self):
        # A case's block is its threat and how the box stops it. The owner rejected a figure set
        # inside it (2026-10-05): a figure goes above or below the block, never between those lines.
        source = (SITE / "index.html").read_text(encoding="utf-8")
        cases = re.findall(r'<li class="attack" data-attack="([\w-]+)">(.*?)</li>', source, re.S)
        self.assertGreaterEqual(len(cases), 7)
        for name, case in cases:
            with self.subTest(case=name):
                block = re.search(r'<div class="case">(.*?)</div>', case, re.S)
                self.assertIsNotNone(block, "a case without its .case block")
                self.assertIn('class="threat"', block.group(1))
                self.assertNotIn('class="evidence"', block.group(1), "a figure inside the case's block")

    def test_a_chapter_gives_every_case_a_figure_or_none(self):
        # Pinned, the figure of the case on screen shows above the rail. A case without one left that
        # space empty, and the owner rejected the gap (2026-10-05).
        source = (SITE / "index.html").read_text(encoding="utf-8")
        chapters = re.findall(r'<ol class="attacks"[^>]*>(.*?)</ol>', source, re.S)
        self.assertGreaterEqual(len(chapters), 2)
        for chapter in chapters:
            cases = re.findall(r'<li class="attack" data-attack="([\w-]+)">(.*?)</li>', chapter, re.S)
            bare = [name for name, case in cases if 'class="evidence"' not in case]
            with self.subTest(cases=[name for name, _ in cases]):
                self.assertIn(len(bare), (0, len(cases)), f"only some cases have a figure; missing: {bare}")

    def test_install_script_is_published_from_the_repo_not_copied(self):
        # coop.dryga.com/install.sh is the repo's install.sh, copied in by the Pages job at deploy.
        # A committed site/install.sh would drift from it, and a deploy that skipped install.sh
        # changes would serve a stale installer.
        self.assertFalse((SITE / "install.sh").exists(), "site/install.sh must not be committed")
        workflow = (SITE.parent / ".github" / "workflows" / "pages.yml").read_text(encoding="utf-8")
        self.assertIn("run: cp install.sh site/install.sh", workflow)
        self.assertIn("'install.sh'", workflow, "an install.sh change must redeploy the site")

    def test_terminals_match_their_cli_sources(self):
        self.assertEqual(gen_site.terminal_drift(), [])

    def test_terminal_drift_names_the_page_block_and_line(self):
        # Drift on each side: the CLI rewords a line the loop shows, the doctor's transcript loses a
        # check, and the homepage shows a fork line the CLI never prints.
        def drifted(rel):
            text = (gen_site.ROOT / rel).read_text()
            if str(rel) == "internal/loop/report.go":
                return text.replace("Task completed: ", "Finished: ")
            if str(rel).endswith("18a-doctor-all-passed.txt"):
                return text.replace("  ✓ .envrc is hidden\n", "")
            if str(rel) == "site/index.html":
                return text.replace("Rebasing onto main", "Replaying onto main")
            return text

        problems = gen_site.terminal_drift(drifted)
        for want in ("index.html, block loop: shape 'Task completed: {}' needs 'Task completed: '",
                     "docs.html, block loop: shape 'Task completed: {}' needs 'Task completed: '",
                     "docs.html, block doctor: line '  ✓ .envrc is hidden' is not in internal/cli/testdata/approved/18a-doctor-all-passed.txt",
                     "index.html, block fork: line 'Replaying onto main' matches no shape listed for it"):
            with self.subTest(want=want):
                self.assertTrue(any(problem.startswith(want) for problem in problems), "\n".join(problems))

    def test_pages_share_the_main_navigation(self):
        for path, install in ((SITE / "index.html", "#start"), (SITE / "overnight.html", "#start"), (SITE / "docs.html", "./#start")):
            with self.subTest(page=path.name):
                page = Page(path)
                for href in ("overnight.html", "docs.html", "https://github.com/AndrewDryga/coop", install):
                    self.assertIn(href, page.main_nav)
                self.assertTrue(any(tag == "a" and attrs.get("href") == "#main" for tag, attrs in page.elements), "missing skip link")
                self.assertTrue(any(attrs.get("id") == "main" for _, attrs in page.elements), "missing #main")

    def test_links_off_the_site_open_in_a_new_tab_with_nofollow(self):
        for path in (SITE / "index.html", SITE / "overnight.html", SITE / "docs.html"):
            links = [attrs for tag, attrs in Page(path).elements if tag == "a" and attrs.get("href")]
            external = [a for a in links if re.match(r"https?://(?!coop\.dryga\.com[/:]|coop\.dryga\.com$)", a["href"])]
            with self.subTest(page=path.name):
                self.assertTrue(external, "the page should cite something")
                for attrs in external:
                    rel = (attrs.get("rel") or "").split()
                    self.assertEqual(attrs.get("target"), "_blank", attrs["href"])
                    self.assertTrue({"nofollow", "noopener"} <= set(rel), attrs["href"])
                for attrs in links:
                    if attrs not in external:
                        self.assertNotIn("target", attrs, attrs["href"])

    def test_faded_case_figures_cannot_take_clicks(self):
        # The story stacks a chapter's case figures in one cell and fades all but the current one. At
        # opacity 0 alone, a later figure stayed on top and took the clicks meant for the visible
        # figure's link, so a faded figure must be hidden as well.
        css = (SITE / "assets" / "css" / "site.css").read_text(encoding="utf-8")
        faded = re.search(r"\.js-story \.case-figures > \.evidence \{([^}]*)\}", css).group(1)
        current = re.search(r"\.js-story \.case-figures > \.is-active \{([^}]*)\}", css).group(1)
        self.assertIn("visibility: hidden", faded)
        self.assertIn("visibility: visible", current)

    def test_external_link_marking_keeps_rel_and_settles(self):
        page = "\n".join([
            '<a href="https://github.com/x">a</a>',
            '<a class="c" rel="me" target="_self" href="http://example.org/">b</a>',
            "<a href='https://example.com/q?a=1'>c</a>",
            '<a href="https://coop.dryga.com/docs.html">d</a>',
            '<a href="docs.html#loop">e</a> <a href="#main">f</a> <abbr title="https://x.org">g</abbr>',
        ])
        marked = gen_site.external_links(page)
        self.assertEqual(marked.splitlines(), [
            '<a href="https://github.com/x" target="_blank" rel="nofollow noopener">a</a>',
            '<a class="c" rel="me nofollow noopener" target="_blank" href="http://example.org/">b</a>',
            "<a href='https://example.com/q?a=1' target=\"_blank\" rel=\"nofollow noopener\">c</a>",
            '<a href="https://coop.dryga.com/docs.html">d</a>',
            '<a href="docs.html#loop">e</a> <a href="#main">f</a> <abbr title="https://x.org">g</abbr>',
        ])
        self.assertEqual(gen_site.external_links(marked), marked)

    def test_sitemap_lists_every_page(self):
        listed = set(re.findall(r"<loc>https://coop\.dryga\.com/([^<]*)</loc>", (SITE / "sitemap.xml").read_text()))
        for path in sorted(SITE.glob("*.html")):
            with self.subTest(page=path.name):
                self.assertIn("" if path.name == "index.html" else path.name, listed)

    def test_task_queue_leads_to_the_overnight_page(self):
        page = (SITE / "index.html").read_text()
        row = re.search(r"<h3>Task queue</h3>(?s:.*?)</li>", page)
        self.assertIsNotNone(row, "the homepage lost its Task queue feature")
        self.assertIn('href="overnight.html"', row.group(0))

    def test_every_start_command_follows_the_agent_picker(self):
        script = (SITE / "assets" / "js" / "site.js").read_text()
        handled = set(re.findall(r'\["(\w+)", `coop', script))
        for path in (SITE / "index.html", SITE / "overnight.html"):
            with self.subTest(page=path.name):
                used = {attrs["data-cmd"] for _, attrs in Page(path).elements if "data-cmd" in attrs}
                self.assertTrue(used, "the page has no start commands")
                self.assertLessEqual(used, handled)

    def test_overnight_board_reads_whole_without_the_script(self):
        page = (SITE / "overnight.html").read_text()
        board = re.search(r'<div class="board"(?s:.*?)<template id="night-task">', page).group(0)
        dots = re.findall(r'<li data-state="(\w+)"></li>', board)
        self.assertEqual(len(dots), -(-gen_site.NIGHT_N // gen_site.NIGHT_PER_DOT))
        self.assertEqual(dots.count("waiting"), 1, "the morning keeps the one decision in sight")
        self.assertIn("120 tasks done. One decision for you.", board)
        self.assertIn('data-open', re.search(r'<ol class="lane waiting">(?s:.*?)</ol>', board).group(0))
        timeline = html.unescape(re.search(r'<script type="application/json" id="night-timeline">(.*?)</script>', page).group(1))
        states = __import__("json").loads(timeline)["states"]
        self.assertEqual(len(states), 1 + len(re.findall(r'<li class="night-step">', page)), "one state for the hero and one per step")

    def test_built_pages_match_their_generator(self):
        generator = SITE.parent / "tools" / "gen_site.py"
        checked = subprocess.run([sys.executable, str(generator), "--check"], capture_output=True, text=True, check=False)
        self.assertEqual(checked.returncode, 0, checked.stderr)


if __name__ == "__main__":
    unittest.main()
