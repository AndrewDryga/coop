import html
import re
import subprocess
import sys
import unittest
from html.parser import HTMLParser
from pathlib import Path


SITE = Path(__file__).resolve().parents[1] / "site"


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

    def test_install_script_is_published_from_the_repo_not_copied(self):
        # coop.dryga.com/install.sh is the repo's install.sh, copied in by the Pages job at deploy.
        # A committed site/install.sh would drift from it, and a deploy that skipped install.sh
        # changes would serve a stale installer.
        self.assertFalse((SITE / "install.sh").exists(), "site/install.sh must not be committed")
        workflow = (SITE.parent / ".github" / "workflows" / "pages.yml").read_text(encoding="utf-8")
        self.assertIn("run: cp install.sh site/install.sh", workflow)
        self.assertIn("'install.sh'", workflow, "an install.sh change must redeploy the site")

    def test_pages_share_the_main_navigation(self):
        for path, install in ((SITE / "index.html", "#start"), (SITE / "docs.html", "./#start")):
            with self.subTest(page=path.name):
                page = Page(path)
                for href in ("docs.html", "https://github.com/AndrewDryga/coop", install):
                    self.assertIn(href, page.main_nav)
                self.assertTrue(any(tag == "a" and attrs.get("href") == "#main" for tag, attrs in page.elements), "missing skip link")
                self.assertTrue(any(attrs.get("id") == "main" for _, attrs in page.elements), "missing #main")

    def test_built_pages_match_their_generator(self):
        generator = SITE.parent / "tools" / "gen_site.py"
        checked = subprocess.run([sys.executable, str(generator), "--check"], capture_output=True, text=True, check=False)
        self.assertEqual(checked.returncode, 0, checked.stderr)


if __name__ == "__main__":
    unittest.main()
