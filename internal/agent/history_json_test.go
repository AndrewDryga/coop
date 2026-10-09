package agent

import (
	"strings"
	"testing"
)

func TestNativeHistoryIndexKeysIgnoreRowTerminator(t *testing.T) {
	var keys []string
	for _, data := range []string{`{"text":"same"}`, "{\"text\":\"same\"}\n"} {
		if err := NativeHistoryIndexRows(strings.NewReader(data), "", func(key string, _, _ int64) error { keys = append(keys, key); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatal("row terminator changed deletion receipt identity")
	}
}

func TestNativeHistoryRowsStreamsLargePayloadsAndPreservesSpans(t *testing.T) {
	large := strings.Repeat("x", 2<<20)
	rows := []string{
		` {"cwd":"/owned","payload":"` + large + `","sessionId":"first"}` + "\r\n",
		`{"nested":[{"payload":"` + large + `"}],"cwd":"/foreign"}` + "\n",
		`{"payload":"` + large + `","cwd":"/owned","sessionId":"last"}`,
	}
	input := strings.Join(rows, "")
	var seen int
	err := nativeHistoryRows(strings.NewReader(input), []string{"cwd", "sessionId"}, func(fields map[string]string, offset, size int64) error {
		if input[offset:offset+size] != rows[seen] {
			t.Fatalf("row %d lost native bytes", seen)
		}
		if fields["cwd"] != []string{"/owned", "/foreign", "/owned"}[seen] {
			t.Fatalf("ownership not extracted: %+v", fields)
		}
		seen++
		return nil
	})
	if err != nil || seen != len(rows) {
		t.Fatalf("rows=%d, err=%v", seen, err)
	}
}

func TestNativeHistoryRowsRejectsAmbiguousMetadata(t *testing.T) {
	for _, input := range []string{
		`{"cwd":"/a","cwd":"/b"}`,
		`{"cwd":"/a","cw\u0064":"/b"}`,
		`{"cwd":"/a","payload":"bad\q"}`,
		`{"cwd":"/a","payload":[1,]}`,
		`{"cwd":"/a","payload":01}`,
		`{"cwd":"/a","payload":1e}`,
		`{"cwd":false}`,
		`{"cwd":"/a"} trailing`,
	} {
		if err := nativeHistoryRows(strings.NewReader(input), []string{"cwd"}, func(map[string]string, int64, int64) error { return nil }); err == nil {
			t.Errorf("invalid metadata accepted: %s", input)
		}
	}
	input := "\n" + `{"cwd":"/a\\b","unknown":[true,false,null,-12.25e+3,{"value":"escaped\"text"}]}` + "\r\n"
	if err := nativeHistoryRows(strings.NewReader(input), []string{"cwd"}, func(fields map[string]string, _, _ int64) error {
		if fields["cwd"] != `/a\b` {
			t.Fatalf("escape decoding: %q", fields["cwd"])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
