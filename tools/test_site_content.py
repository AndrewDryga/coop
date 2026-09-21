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
        if self.code_id:
            self.codes[self.code_id] += data


class SiteContentTest(unittest.TestCase):
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
