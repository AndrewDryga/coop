import re
import subprocess
import unittest
from html.parser import HTMLParser
from pathlib import Path


SITE = Path(__file__).resolve().parents[1] / "site"


class Page(HTMLParser):
    def __init__(self, path):
        super().__init__()
        self.elements = []
        self.codes = {}
        self.text = []
        self.code_id = None
        self.feed(path.read_text(encoding="utf-8"))

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        self.elements.append((tag, attrs))
        if tag == "code" and "id" in attrs:
            self.code_id = attrs["id"]
            self.codes[self.code_id] = ""

    def handle_endtag(self, tag):
        if tag == "code":
            self.code_id = None

    def handle_data(self, data):
        self.text.append(data)
        if self.code_id:
            self.codes[self.code_id] += data


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

    def test_installer_copy_targets_are_executable_one_liners(self):
        page = Page(SITE / "index.html")
        targets = [attrs["data-copy"] for _, attrs in page.elements if "data-copy" in attrs]
        self.assertEqual(len(targets), 2)
        expected = "curl -fsSL https://raw.githubusercontent.com/AndrewDryga/coop/main/install.sh | sh"
        for target in targets:
            with self.subTest(target=target):
                copied = page.codes[target.removeprefix("#")].strip()
                parsed = subprocess.run(["sh", "-n"], input=copied, text=True, capture_output=True, check=False)
                self.assertEqual(parsed.returncode, 0, parsed.stderr)
                self.assertEqual(copied, expected)

    def test_mobile_menus_name_their_controlled_navigation(self):
        for path in (SITE / "index.html", SITE / "docs.html"):
            with self.subTest(page=path.name):
                page = Page(path)
                toggles = [attrs for tag, attrs in page.elements if tag == "button" and "data-nav-toggle" in attrs]
                navs = [attrs for tag, attrs in page.elements if tag == "nav" and "data-nav" in attrs]
                self.assertEqual(len(toggles), 1)
                self.assertEqual(len(navs), 1)
                self.assertEqual(toggles[0].get("aria-expanded"), "false")
                self.assertTrue(navs[0].get("id"))
                self.assertEqual(toggles[0].get("aria-controls"), navs[0]["id"])
                self.assertTrue(toggles[0].get("aria-label"))


if __name__ == "__main__":
    unittest.main()
