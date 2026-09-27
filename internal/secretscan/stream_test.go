package secretscan

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCredentialStreamHasNoLineOrBinaryEscape(t *testing.T) {
	for _, token := range []string{
		"ghp_" + strings.Repeat("aB7C", 5),
		"XOXB-" + strings.Repeat("aB7C", 4),
		"xapp-" + strings.Repeat("aB7C", 4),
		"emk-" + strings.Repeat("aB7C", 4),
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"eyJ" + strings.Repeat("a", 200_000) + ".eyJ" + strings.Repeat("B", 200_000) + ".sig",
	} {
		// Invalid UTF-8 and a split token must not turn the rest into an unscanned blob.
		input := io.MultiReader(bytes.NewReader(bytes.Repeat([]byte{0, 0xff}, 50_000)), strings.NewReader("\n"+token+"\n"))
		found, err := ContainsCredential(&shortCredentialReads{input})
		if err != nil || !found {
			t.Fatalf("missed credential shape %.12q: found=%v err=%v", token, found, err)
		}
	}
	for _, text := range []string{strings.Repeat("ordinary data\x00\xff", 100_000), "token = var.my_long_token_reference"} {
		if found, err := ContainsCredential(strings.NewReader(text)); found || err != nil {
			t.Fatalf("clean content refused: found=%v err=%v", found, err)
		}
	}
}

type shortCredentialReads struct{ io.Reader }

func (reader *shortCredentialReads) Read(data []byte) (int, error) {
	return reader.Reader.Read(data[:min(len(data), 3)])
}

type failedCredentialRead struct{}

var errCredentialRead = errors.New("broken source")

func (failedCredentialRead) Read([]byte) (int, error) { return 0, errCredentialRead }

func TestCredentialStreamReadFailureIsNotACompletedCleanScan(t *testing.T) {
	for _, prefix := range []string{"ordinary", strings.Repeat("a", 200_000)} {
		_, err := ContainsCredential(io.MultiReader(strings.NewReader(prefix), failedCredentialRead{}))
		if !errors.Is(err, errCredentialRead) {
			t.Fatalf("read failure hidden: %v", err)
		}
	}
}

func TestCredentialStreamLineBoundariesPreserveTheWholePattern(t *testing.T) {
	token := "ghp_" + strings.Repeat("a", 40)
	for _, prefix := range []string{"", strings.Repeat("a", 64<<10-8), strings.Repeat("a", 200_000)} {
		for _, suffix := range []string{"", "\n", "\n" + strings.Repeat("b", 200_000)} {
			input := prefix + "\n" + token + suffix
			want := credentialStreamPattern.MatchString(input)
			got, err := ContainsCredential(strings.NewReader(input))
			if err != nil || got != want {
				t.Fatalf("prefix=%d suffix=%d: found=%v want=%v err=%v", len(prefix), len(suffix), got, want, err)
			}
		}
	}
	for _, input := range []string{"sk-\n" + strings.Repeat("a", 50), "ghp_" + strings.Repeat("a", 200_000) + "_"} {
		if got, err := ContainsCredential(strings.NewReader(input)); got || err != nil {
			t.Fatalf("non-credential matched: %v %v", got, err)
		}
	}
}

func TestCredentialStreamDrainsAndReportsErrorsAfterAMatch(t *testing.T) {
	for _, text := range []string{
		"sk-" + strings.Repeat("a", 30) + "\nremaining bytes",
		"sk-" + strings.Repeat("a", 200_000),
	} {
		var captured bytes.Buffer
		found, err := ContainsCredential(io.TeeReader(strings.NewReader(text), &captured))
		if !found || err != nil || captured.String() != text {
			t.Fatalf("match failed to drain: found=%v err=%v bytes=%d", found, err, captured.Len())
		}
		found, err = ContainsCredential(&terminalCredentialRead{data: []byte(text)})
		if !found || !errors.Is(err, errCredentialRead) {
			t.Fatalf("error after match hidden: found=%v err=%v", found, err)
		}
	}
}

type terminalCredentialRead struct{ data []byte }

func (reader *terminalCredentialRead) Read(data []byte) (int, error) {
	n := copy(data, reader.data)
	reader.data = reader.data[n:]
	if len(reader.data) == 0 {
		return n, errCredentialRead
	}
	return n, nil
}

func BenchmarkCredentialStream(b *testing.B) {
	for name, content := range map[string][]byte{
		"lines":  bytes.Repeat([]byte("large review line\n"), (66<<20)/18),
		"binary": bytes.Repeat([]byte{0, 0xff, 'a', 'b'}, (66<<20)/4),
	} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(content)))
			b.ReportAllocs()
			for b.Loop() {
				if found, err := ContainsCredential(bytes.NewReader(content)); found || err != nil {
					b.Fatalf("clean content refused: %v %v", found, err)
				}
			}
		})
	}
}
