package networkgateway

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
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
		transient  bool
	}{
		{"truncated", `{"Revision":1`, nil, true},
		{"empty", "", nil, true},
		{"unknown-field", `{"Unexpected":true}`, nil, false},
		{"wrong-type", `{"Revision":"bad"}`, nil, false},
		{"missing-file", "", os.ErrNotExist, true},
		{"read-error", "", errors.New("test-only read refusal"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 125*time.Millisecond)
			defer cancel()
			reads := 0
			var snapshot NativeAccessSnapshot
			err := decodeNativeDocument(ctx, &snapshot, func() ([]byte, error) { reads++; return []byte(test.data), test.readError })
			if err == nil || reads == 0 || strings.Contains(err.Error(), test.data) && test.data != "" {
				t.Fatalf("refusal reads%d error%v", reads, err)
			}
			if test.transient {
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) || reads > 3 || snapshot != (NativeAccessSnapshot{}) {
					t.Fatalf("persistent refusal: reads%d deadline%v snapshot_zero%t", reads, ctx.Err(), snapshot == (NativeAccessSnapshot{}))
				}
			} else if reads != 1 {
				t.Fatalf("semantic or I/O refusal retried: reads%d", reads)
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

func TestNativeDocumentUsesLoadDeadlineForVMConvergence(t *testing.T) {
	for _, missing := range []bool{false, true} {
		reads := 0
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var snapshot NativeAccessSnapshot
		err := decodeNativeDocument(ctx, &snapshot, func() ([]byte, error) {
			reads++
			if reads <= 3 {
				if missing {
					return nil, os.ErrNotExist
				}
				return []byte(`{"Revision":1,"Credential":"partial-test-only"`), nil
			}
			return []byte(`{"Revision":2,"Revoked":true}`), nil
		})
		cancel()
		if err != nil || reads != 4 || snapshot.Revision != 2 || !snapshot.Revoked || snapshot.Credential != "" {
			t.Errorf("missing%t fresh revision%d revoked%t credential%t reads%d error%v", missing, snapshot.Revision, snapshot.Revoked, snapshot.Credential != "", reads, err)
		}
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

func TestNativeDocumentBackgroundRetryIsBounded(t *testing.T) {
	reads := 0
	var snapshot NativeAccessSnapshot
	err := decodeNativeDocument(context.Background(), &snapshot, func() ([]byte, error) { reads++; return nil, os.ErrNotExist })
	// The retry budget is one second even without a caller deadline. At the
	// 50ms cadence it permits at most 20 reads; scheduling may permit fewer.
	if err == nil || reads < 1 || reads > 20 || snapshot != (NativeAccessSnapshot{}) {
		t.Fatalf("background refusal: reads%d error%v snapshot_zero%t", reads, err, snapshot == (NativeAccessSnapshot{}))
	}
}

type nativeDocumentCancelOnDecode struct{ cancel context.CancelFunc }

func (v *nativeDocumentCancelOnDecode) UnmarshalJSON([]byte) error {
	v.cancel()
	return nil
}

func TestNativeDocumentRefusesCancellationDuringDecode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	value := nativeDocumentCancelOnDecode{cancel: cancel}
	if decodeNativeDocument(ctx, &value, func() ([]byte, error) { return []byte(`{}`), nil }) == nil {
		t.Fatal("accepted a document after cancellation during decode")
	}
}
