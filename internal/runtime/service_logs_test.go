package runtime

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

func TestServiceLogCursorPreservesBothStreamsAndRefusesRotation(t *testing.T) {
	var logs bytes.Buffer
	const stamp = "2026-11-27T12:08:02.123456789Z"
	container := strings.Repeat("a", 64)
	for i := range 141 {
		line := fmt.Sprintf("%s line %03d\n", stamp, i)
		header := [8]byte{byte(1 + i%2)}
		binary.BigEndian.PutUint32(header[4:], uint32(len(line)))
		logs.Write(header[:])
		logs.WriteString(line)
	}
	first, err := readServiceLogPage(bytes.NewReader(logs.Bytes()), serviceLogCursor{Container: container})
	if err != nil || len(first.Records) != 128 || first.AtEnd || first.Records[1].Stream != "stderr" {
		t.Fatalf("first page: %d, %v", len(first.Records), err)
	}
	cursor, err := decodeServiceLogCursor(container, first.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	next, err := readServiceLogPage(bytes.NewReader(logs.Bytes()), cursor)
	if err != nil || !next.AtEnd || len(next.Records) != 13 || next.Records[12].Text != "line 140\n" {
		t.Fatalf("next page: %+v, %v", next, err)
	}
	if _, err := decodeServiceLogCursor(strings.Repeat("b", 64), first.Cursor); err == nil {
		t.Fatal("accepted a continuation for another container")
	}
	rotated := bytes.Replace(logs.Bytes(), []byte("line 000"), []byte("line 001"), 1)
	if _, err := readServiceLogPage(bytes.NewReader(rotated), cursor); err == nil {
		t.Fatal("accepted a rotated log")
	}
}
