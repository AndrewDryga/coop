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
    def test_evaluations_are_navigable_and_use_readable_code_blocks(self):
        page = Page(SITE / "docs.html")
        self.assertTrue(any(tag == "a" and attrs.get("href") == "#evals" for tag, attrs in page.elements))
        self.assertEqual(sum(tag == "section" and attrs.get("id") == "evals" for tag, attrs in page.elements), 1)
        source = (SITE / "docs.html").read_text(encoding="utf-8")
        section = source.split('id="evals">', 1)[1].split("</section>", 1)[0]
        blocks = re.findall(r'<pre([^>]*)>(.*?)</pre>', section, re.S)
        self.assertEqual(len(blocks), 2)
        for attrs, _ in blocks:
            self.assertIn('class="code"', attrs)
        self.assertIn("--dry-run", blocks[0][1])

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
            "curl -fsSL https://raw.githubusercontent.com/AndrewDryga/coop/main/install.sh | sh",
            "coop login claude",
            "cd your-project && coop init",
            "coop claude",
        ])
        for command in commands:
            with self.subTest(command=command):
                parsed = subprocess.run(["sh", "-n"], input=command, text=True, capture_output=True, check=False)
                self.assertEqual(parsed.returncode, 0, parsed.stderr)

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
