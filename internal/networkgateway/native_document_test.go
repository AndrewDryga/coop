package networkgateway

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestNativeDocumentReopensTruncatedVirtualFilesystemRead(t *testing.T) {
	reads := 0
	var snapshot NativeAccessSnapshot
	err := decodeNativeDocument(context.Background(), &snapshot, func() ([]byte, error) {
		reads++
		if reads == 1 {
			// Captured VM read returned a complete new prefix with the closing
			// bytes missing, although the host published a complete atomic file.
			return []byte(`{"Revision":1,"Credential":"stale-partial"`), nil
		}
		return []byte(`{"Revision":2,"Revoked":true}`), nil
	})
	if err != nil || reads != 2 || snapshot.Revision != 2 || !snapshot.Revoked || snapshot.Credential != "" {
		t.Fatalf("fresh read = revision%d revoked%t inherited_credential%t reads%d error%v", snapshot.Revision, snapshot.Revoked, snapshot.Credential != "", reads, err)
	}
}

func TestNativeDocumentPersistentAndSemanticFailuresRemainClosed(t *testing.T) {
	for _, test := range []struct {
		name, data string
		readError  error
		wantReads  int
	}{
		{"truncated", `{"Revision":1`, nil, 3},
		{"empty", "", nil, 3},
		{"unknown-field", `{"Unexpected":true}`, nil, 1},
		{"wrong-type", `{"Revision":"bad"}`, nil, 1},
		{"missing-file", "", os.ErrNotExist, 3},
		{"read-error", "", errors.New("test-only read refusal"), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			var snapshot NativeAccessSnapshot
			err := decodeNativeDocument(context.Background(), &snapshot, func() ([]byte, error) { reads++; return []byte(test.data), test.readError })
			if err == nil || reads != test.wantReads || strings.Contains(err.Error(), test.data) && test.data != "" {
				t.Fatalf("refusal reads%d error%v", reads, err)
			}
		})
	}
}

func TestNativeDocumentReopensTransientlyMissingVirtualFilesystemLeaf(t *testing.T) {
	reads := 0
	var snapshot NativeAccessSnapshot
	err := decodeNativeDocument(context.Background(), &snapshot, func() ([]byte, error) {
		reads++
		if reads == 1 {
			return nil, os.ErrNotExist
		}
		return []byte(`{"Revision":2,"Revoked":true}`), nil
	})
	if err != nil || reads != 2 || snapshot.Revision != 2 || !snapshot.Revoked || snapshot.Credential != "" {
		t.Fatalf("fresh missing-leaf read = revision%d revoked%t credential%t reads%d error%v", snapshot.Revision, snapshot.Revoked, snapshot.Credential != "", reads, err)
	}
}

func TestNativeDocumentRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	var snapshot NativeAccessSnapshot
	err := decodeNativeDocument(ctx, &snapshot, func() ([]byte, error) { reads++; cancel(); return []byte(`{"Revision":`), nil })
	if err == nil || reads != 1 {
		t.Fatalf("canceled read retried: reads%d error%v", reads, err)
	}
}
