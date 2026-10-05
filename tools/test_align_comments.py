import importlib.util
import pathlib
import tempfile
import unittest

_spec = importlib.util.spec_from_file_location("align_comments", pathlib.Path(__file__).with_name("align-comments.py"))
align = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(align)


def offenders(name, text):
    with tempfile.TemporaryDirectory() as tmp:
        path = pathlib.Path(tmp) / name
        path.write_text(text)
        return align.process(str(path), False)


class AlignCommentsTest(unittest.TestCase):
    def test_markdown_fence_counts_placeholders_as_text(self):
        # GitHub shows <id> inside a fence, so it takes four columns there.
        aligned = "```bash\ncoop context --task <id>  # one\ncoop context --changed    # two\n```\n"
        self.assertEqual(offenders("a.md", aligned), [])
        tag_measured = "```bash\ncoop context --task <id> # one\ncoop context --changed    # two\n```\n"
        self.assertEqual(len(offenders("b.md", tag_measured)), 2, "a line aligned as if <id> were a tag is off by four")

    def test_html_pre_still_ignores_markup(self):
        # In HTML, <span> is markup and &lt;id&gt; shows as <id>: both measure by what the reader sees.
        html = ('<pre class="code">\n'
                '<span class="k">coop</span> context --task &lt;id&gt;  <span class="t"># one</span>\n'
                'coop context --changed    <span class="t"># two</span>\n'
                '</pre>\n')
        self.assertEqual(offenders("a.html", html), [])


if __name__ == "__main__":
    unittest.main()
