package fsfilter

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
	"sync"
	"testing"
)

var testUnicode = sync.OnceValues(UnicodeData)

func testUnicodeData(t *testing.T) []byte {
	t.Helper()
	data, err := testUnicode()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestKernelPolicySeparatesRootsAndIdentityGrants(t *testing.T) {
	policies := []Policy{
		{Profile: Profile{Fold: true}, Entries: []Entry{{"private", Secret}, {"private/allowed", Secret}}, Grants: []string{"private/allowed"}},
		{Profile: Profile{Normalize: true}, Entries: []Entry{{".coopignore", ReadOnly}}},
	}
	var out bytes.Buffer
	if err := WriteKernelPolicies(&out, policies, testUnicodeData(t)); err != nil {
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
	}{{1, Entry{"private", Secret | Custody}}, {1, Entry{"private/allowed", Secret}}, {2, Entry{".coopignore", ReadOnly}}, {1, Entry{"private/allowed", ReadOnly | 16}}} {
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
		if err := WriteKernelPolicies(io.Discard, policies, testUnicodeData(t)); err == nil {
			t.Fatal("accepted invalid policy", policies)
		}
	}
}

func TestKernelPinPlanDeduplicatesAncestorsAndKeepsEveryGrantLast(t *testing.T) {
	policies := []Policy{
		{Entries: []Entry{{"a/b/key", Secret}, {"a/b/policy", ReadOnly}}, Grants: []string{"a/c/grant"}},
		{Entries: []Entry{{"a", Secret}}, Grants: []string{"a/grant"}},
	}
	var out bytes.Buffer
	if err := WriteKernelPolicies(&out, policies, testUnicodeData(t)); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(out.Bytes()[8:])
	var counts [5]uint32
	if err := binary.Read(r, binary.LittleEndian, &counts); err != nil {
		t.Fatal(err)
	}
	if counts[3] != 6 || counts[4] != 2 {
		t.Fatal(counts)
	}
	if _, err := r.Seek(int64(counts[0])*8+int64(counts[1])*28+int64(counts[2])*48, io.SeekCurrent); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		root  uint32
		path  string
		flags uint32
	}{
		{1, "a", Custody}, {1, "a/b", Custody}, {1, "a/b/key", Secret}, {1, "a/b/policy", ReadOnly}, {1, "a/c", Custody},
		{2, "a", Secret | Custody}, {1, "a/c/grant", ReadOnly | 16}, {2, "a/grant", ReadOnly | 16},
	}
	for _, expected := range want {
		var row [3]uint32
		if err := binary.Read(r, binary.LittleEndian, &row); err != nil {
			t.Fatal(err)
		}
		name := make([]byte, row[1])
		if _, err := io.ReadFull(r, name); err != nil {
			t.Fatal(err)
		}
		if row[0] != expected.root || string(name) != expected.path || row[2] != expected.flags {
			t.Fatalf("pin row %v %q, want %+v", row, name, expected)
		}
	}
	if r.Len() != 0 {
		t.Fatal("unexpected pin rows")
	}
}

func TestKernelPolicyRejectsBrokenUnicodeData(t *testing.T) {
	for _, data := range [][]byte{nil, make([]byte, 47), make([]byte, 20001*48)} {
		if err := WriteKernelPolicy(io.Discard, Policy{}, data); err == nil {
			t.Fatal("accepted malformed table")
		}
	}
}

func TestGeneratedUnicodeDataIsDeterministic(t *testing.T) {
	data, err := UnicodeData()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, testUnicodeData(t)) {
		t.Fatal("Unicode table generation is nondeterministic")
	}
	table, err := UnicodeTable()
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(data)
	var previous uint32
	for r.Len() > 0 {
		var key uint32
		var value RuneMapping
		if err := binary.Read(r, binary.LittleEndian, &key); err != nil {
			t.Fatal(err)
		}
		if err := binary.Read(r, binary.LittleEndian, &value); err != nil {
			t.Fatal(err)
		}
		if key <= previous || table[key] != value {
			t.Fatalf("incorrect Unicode row U+%04X", key)
		}
		previous = key
		delete(table, key)
	}
	if len(table) != 0 {
		t.Fatalf("missing %d Unicode mappings", len(table))
	}
}
