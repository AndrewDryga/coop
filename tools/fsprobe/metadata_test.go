package main

import (
	"encoding/hex"
	"testing"
)

func TestOpenTelemetryRejectsIncompleteOrAmbiguousEvidence(t *testing.T) {
	good := openRecord{PathHex: hex.EncodeToString([]byte("/workspace/warm-add")), FirstInode: 1, LastInode: 1, FirstLinks: 1, LastLinks: 2, Opens: 2, Device: 36}
	values, err := decodeTelemetry(raw([]openRecord{good}))
	if err != nil || values["/workspace/warm-add"].LastLinks != 2 || values["/workspace/warm-add"].FirstLinks != 1 {
		t.Fatalf("metadata transition lost: %v %v", values, err)
	}
	for _, data := range [][]byte{[]byte("null"), []byte("[]"), []byte("["), raw([]openRecord{good, good}), raw(make([]openRecord, 512))} {
		if _, err := decodeTelemetry(data); err == nil {
			t.Fatalf("accepted incomplete/ambiguous telemetry: %.100s", data)
		}
	}
	for _, path := range []string{"", "not-hex", hex.EncodeToString([]byte("relative")), hex.EncodeToString(make([]byte, 256))} {
		bad := good
		bad.PathHex = path
		if _, err := decodeTelemetry(raw([]openRecord{bad})); err == nil {
			t.Fatalf("accepted invalid path: %q", path)
		}
	}
	for _, field := range []string{"first", "last", "opens"} {
		bad := good
		switch field {
		case "first":
			bad.FirstInode = 0
		case "last":
			bad.LastInode = 0
		case "opens":
			bad.Opens = 0
		}
		if _, err := decodeTelemetry(raw([]openRecord{bad})); err == nil {
			t.Fatalf("accepted missing %s", field)
		}
	}
}

func TestNewProbeArtifactsRemainInsideFixtureManifest(t *testing.T) {
	for _, base := range []string{"workspace", "workspace/nested", "workspace/old-nested"} {
		for _, name := range []string{"warm-add", "warm-remove", "fresh-host-alias", "public-hard", "public-linked", "guest-alias", "guest-save", "Café", "held-public", "held-ready", "held-release"} {
			if !fixturePath(base + "/" + name) {
				t.Fatalf("missing cleanup ownership: %s/%s", base, name)
			}
		}
	}
}
