package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestInstallerChecksDigestAndExtractsOnlyExecutable(t *testing.T) {
	for _, link := range []bool{false, true} {
		var data bytes.Buffer
		gz := gzip.NewWriter(&data)
		tw := tar.NewWriter(gz)
		kind := byte(tar.TypeReg)
		body := []byte("reviewed executable")
		if link {
			kind = tar.TypeSymlink
			body = nil
		}
		if err := tw.WriteHeader(&tar.Header{Name: "trivy", Typeflag: kind, Size: int64(len(body)), Linkname: "/outside"}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data.Bytes())
		bin, err := verifiedBinary(data.Bytes(), hex.EncodeToString(hash[:]))
		if link && err == nil || !link && (err != nil || !bytes.Equal(bin, body)) {
			t.Fatalf("link=%v binary=%q error=%v", link, bin, err)
		}
		if _, err := verifiedBinary(data.Bytes(), "wrong"); err == nil {
			t.Fatal("unverified archive accepted")
		}
	}
}
