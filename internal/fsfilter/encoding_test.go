package fsfilter

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
	"testing"
)

func TestKernelPolicySeparatesRootsAndIdentityGrants(t *testing.T) {
	policies := []Policy{
		{Profile: Profile{Fold: true}, Entries: []Entry{{"private", Secret}, {"private/allowed", Secret}}, Grants: []string{"private/allowed"}},
		{Profile: Profile{Normalize: true}, Entries: []Entry{{".coopignore", ReadOnly}}},
	}
	var out bytes.Buffer
	if err := WriteKernelPolicies(&out, policies); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(out.Bytes()[8:])
	read := func(v any) {
		t.Helper()
		if err := binary.Read(r, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	var counts [5]uint32
	read(&counts)
	if counts[0] != 2 || counts[3] != 3 || counts[4] != 1 {
		t.Fatal(counts)
	}
	for i, want := range []uint32{2, 1} {
		var profile [2]uint32
		read(&profile)
		if profile != [2]uint32{uint32(i + 1), want} {
			t.Fatal(profile)
		}
	}
	got := map[PathKey]uint32{}
	for range counts[1] {
		var key PathKey
		var flags uint32
		read(&key)
		read(&flags)
		got[key] = flags
		if flags&16 != 0 {
			t.Fatal("hashed path acquired allow authority")
		}
	}
	want := map[PathKey]uint32{}
	for i, p := range policies {
		compiled, err := p.Compile(uint32(i + 1))
		if err != nil {
			t.Fatal(err)
		}
		for k, f := range compiled {
			want[k] = f
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("denials crossed roots or lost flags")
	}
	if _, err := r.Seek(int64(counts[2])*48, io.SeekCurrent); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		root  uint32
		entry Entry
	}{{1, Entry{"private", Secret}}, {1, Entry{"private/allowed", Secret}}, {2, Entry{".coopignore", ReadOnly}}, {1, Entry{"private/allowed", ReadOnly | 16}}} {
		var header [3]uint32
		read(&header)
		name := make([]byte, header[1])
		if _, err := io.ReadFull(r, name); err != nil {
			t.Fatal(err)
		}
		if header[0] != want.root || header[2] != want.entry.Flags || string(name) != want.entry.Path {
			t.Fatalf("unexpected inventory entry %v %q", header, name)
		}
	}
	if r.Len() != 0 {
		t.Fatal("unexpected trailing wire data")
	}
}

func TestKernelPolicyRejectsInvalidRootsAndGrants(t *testing.T) {
	for _, policies := range [][]Policy{nil, make([]Policy, 33), {{Grants: []string{"../outside"}}}, {{Grants: []string{"/absolute"}}}, {{Entries: []Entry{{"key", 16}}}}} {
		if err := WriteKernelPolicies(io.Discard, policies); err == nil {
			t.Fatal("accepted invalid policy", policies)
		}
	}
}
