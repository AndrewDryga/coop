package fsfilter

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func TestAdmissionProtectsAliasesAndAncestorCustody(t *testing.T) {
	p := Policy{Profile: Profile{Normalize: true, Fold: true}, Entries: []Entry{{"config/Café", Secret}, {".coopignore", ReadOnly}}}
	compiled, err := p.Compile(1)
	if err != nil {
		t.Fatal(err)
	}
	keyFor := func(path string) PathKey {
		key := PathKey{A: nameSeedA, B: nameSeedB, Root: 1}
		for _, name := range strings.Split(path, "/") {
			key = appendName(key, NameFingerprint(name, p.Profile))
		}
		return key
	}
	for _, name := range []string{"config/Café", "CONFIG/caFe\u0301", "Config/CAFÉ"} {
		if compiled[keyFor(name)] != Secret {
			t.Fatalf("alias %q lost secret protection", name)
		}
	}
	if compiled[keyFor("config")] != Custody {
		t.Fatal("moving an ancestor would lose the admitted path")
	}
	if compiled[keyFor(".COOPIGNORE")] != ReadOnly {
		t.Fatal("policy should remain readable and immutable")
	}
	for _, name := range []string{"config/public", "other/Café", "config/Cafe"} {
		if compiled[keyFor(name)] != 0 {
			t.Fatalf("public path %q hidden", name)
		}
	}
}

func TestAdmissionKeepsCaseAndNormalizationIndependent(t *testing.T) {
	for _, profile := range []Profile{{}, {Normalize: true}, {Fold: true}, {Normalize: true, Fold: true}} {
		t.Run(string(rune('a'+boolInt(profile.Normalize)*2+boolInt(profile.Fold))), func(t *testing.T) {
			base := NameFingerprint("Café", profile)
			if (base == NameFingerprint("CAFÉ", profile)) != profile.Fold {
				t.Fatal("case sensitivity changed")
			}
			if (base == NameFingerprint("Cafe\u0301", profile)) != profile.Normalize {
				t.Fatal("normalization sensitivity changed")
			}
		})
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestAdmissionMergesRestrictionsAndRejectsIncompletePolicies(t *testing.T) {
	p := Policy{Profile: Profile{Fold: true}, Entries: []Entry{{"KEY", Secret}, {"key", ReadOnly}, {"key/child", Secret}}}
	compiled, err := p.Compile(1)
	if err != nil {
		t.Fatal(err)
	}
	key := appendName(PathKey{A: nameSeedA, B: nameSeedB, Root: 1}, NameFingerprint("key", p.Profile))
	if compiled[key] != Secret|ReadOnly|Custody {
		t.Fatal("equivalent path weakened protection")
	}
	for _, name := range []string{"", ".", "../key", "/key", "a/../key", "a//key", "a\x00b", "a\xffb", strings.Repeat("x", 256), strings.Repeat("d/", MaxDepth) + "key"} {
		if _, err := (Policy{Entries: []Entry{{name, Secret}}}).Compile(1); err == nil {
			t.Fatalf("admitted incomplete/escaping policy %q", name)
		}
	}
	if _, err := p.Compile(0); err == nil {
		t.Fatal("zero root admitted")
	}
}

func TestUnicodeTableMatchesCanonicalCaselessReference(t *testing.T) {
	table, err := UnicodeTable()
	if err != nil {
		t.Fatal(err)
	}
	if len(table) > 20000 {
		t.Fatal("kernel table capacity exceeded")
	}
	// Exercise the same staged operations the kernel performs against x/text's
	// independent string implementation, including ordering before case folding.
	apply := func(input []rune, fold bool) []rune {
		var out []rune
		for _, r := range input {
			v := table[uint32(r)]
			mapped, n := v.Decomposition, v.NDecomposition
			if fold {
				mapped, n = v.Fold, v.NFold
			}
			if n == 0 {
				mapped[0] = uint32(r)
				n = 1
			}
			for _, c := range mapped[:n] {
				at := len(out)
				out = append(out, rune(c))
				if fold {
					continue
				}
				klass := table[c].CCC
				for klass != 0 && at > 0 {
					before := table[uint32(out[at-1])].CCC
					if before == 0 || before <= klass {
						break
					}
					out[at] = out[at-1]
					at--
				}
				out[at] = rune(c)
			}
		}
		return out
	}
	check := func(s string) {
		got := string(apply(apply(apply([]rune(s), false), true), false))
		want := NormalizeName(s, Profile{Normalize: true, Fold: true})
		if got != want {
			t.Fatalf("normalization %q: got %q want %q", s, got, want)
		}
	}
	for r := range table {
		check(string(rune(r)))
	}
	for _, s := range []string{"Cafe\u0301", "CAFÉ", "\u212aEY", "a\u0345\u0301", "\uac01", "\u1100\u1161\u11a8", "Straße", "İ", "Σςσ"} {
		check(s)
	}
	check("a" + strings.Repeat("\u0301\u0323", 60))
	random := rand.New(rand.NewPCG(1, 2))
	alphabet := []rune("aAéÉΩΣςσİKß가각\u0345\u0301\u0323\u0315")
	for i := 0; i < 1000; i++ {
		var s []rune
		for j := 0; j < 16; j++ {
			s = append(s, alphabet[random.IntN(len(alphabet))])
		}
		check(string(s))
		if got, want := NormalizeName(string(s), Profile{Normalize: true, Fold: true}), norm.NFD.String(cases.Fold().String(norm.NFD.String(string(s)))); got != want {
			t.Fatalf("reference mismatch: %q != %q", got, want)
		}
	}
}

func TestCanonicalDecompositionDoesNotInsertStreamSafetyCharacters(t *testing.T) {
	name := "A" + strings.Repeat("\u0301", 80)
	want := "a" + strings.Repeat("\u0301", 80)
	if got := NormalizeName(name, Profile{Normalize: true, Fold: true}); got != want {
		t.Fatalf("filesystem name changed: %q", got)
	}
}

func TestKernelPolicyIsVersionedAndPropagatesWriteFailure(t *testing.T) {
	var out bytes.Buffer
	policy := Policy{Entries: []Entry{{"key", Secret}}}
	if err := WriteKernelPolicies(&out, []Policy{policy}, testUnicodeData(t)); err != nil {
		t.Fatal(err)
	}
	if string(out.Bytes()[:8]) != "COOPFS03" {
		t.Fatal("missing version")
	}
	var counts [5]uint32
	if err := binary.Read(bytes.NewReader(out.Bytes()[8:]), binary.LittleEndian, &counts); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(counts[:2], []uint32{1, 1}) || counts[2] == 0 || counts[3] != 1 || counts[4] != 0 {
		t.Fatalf("invalid section counts %v", counts)
	}
	if err := WriteKernelPolicies(failingWriter{}, []Policy{policy}, testUnicodeData(t)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lost write failure: %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
