package secretname

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"

	"github.com/AndrewDryga/coop/internal/fsfilter"
)

func TestHidden(t *testing.T) {
	for name, want := range map[string]bool{
		".env": true, ".ENV": true, "late.pem": true, "LATE.PEM": true, "id_rsa": true, "id_rsa_late": true,
		"deploy.key": true, "terraform.tfstate": true, "terraform.tfstate.backup": true, ".aws": true,
		"Kelvin.key": true, "İd_rsa": true, "clé.pem": true, "id_rsa.example": true,
		".env.example": false, "cacert.pem": false, "secrets.go": false, "secrets.md": false, "public.txt": false,
		"key.pem.txt": false, "README": false, "env": false,
	} {
		if got := Hidden(name); got != want {
			t.Errorf("Hidden(%q) = %t, want %t", name, got, want)
		}
	}
}

// The guard can only lower what strings.ToLower lowers into ASCII; every other code point is one
// column. A Unicode table that lowered a third code point into ASCII would make the guard miss it.
func TestColumnLowersLikeStringsToLower(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if 0xd800 <= r && r <= 0xdfff {
			continue
		}
		want := otherColumn
		if lower := unicode.ToLower(r); lower < otherColumn {
			want = int(lower)
		}
		if got := column(r); got != want {
			t.Fatalf("column(U+%04X) = %d, want %d", r, got, want)
		}
	}
}

// The table must agree with Hidden on every name, so it is asked about names made from the globs
// themselves (wildcards filled in, cut short, extended, in capitals, with non-ASCII letters) and
// random names over the globs' own bytes and a few code points the guard treats specially.
func TestTableMatchesHidden(t *testing.T) {
	table, err := buildTable()
	if err != nil {
		t.Fatal(err)
	}
	walk := func(name string) bool {
		state := 0
		for _, r := range name {
			state = int(table.next[state][column(r)])
		}
		return table.hidden[state]
	}
	var names []string
	alphabet := []rune("ABZéÉİKßſ世")
	for _, globs := range [][]string{SecretGlobs, AllowGlobs, TemplateGlobs, HardGlobs} {
		for _, glob := range globs {
			alphabet = append(alphabet, []rune(glob)...)
			for _, fill := range []string{"", "x", "a.b", "pem", ".", "é", "世"} {
				filled := strings.NewReplacer("*", fill, "?", "q").Replace(glob)
				for _, name := range []string{filled, "x" + filled, filled + ".bak", filled + "x", filled + ".example", "é" + filled} {
					names = append(names, name, strings.ToUpper(name))
				}
			}
			for i := 0; i <= len(glob); i++ {
				names = append(names, glob[:i], glob[i:])
			}
		}
	}
	random := rand.New(rand.NewPCG(5, 6))
	for range 50000 {
		name := make([]rune, 1+random.IntN(16))
		for i := range name {
			name[i] = alphabet[random.IntN(len(alphabet))]
		}
		names = append(names, string(name))
	}
	// The guard walks the table over a name in the form the volume compares it, decomposed, case-folded
	// or both (fsfilter.NormalizeName), so every name is also checked in those forms.
	names = append(names, "K.PEM", "K.PEM", "Kelvin.key", "clé.pem", "clé.pem", "STRAẞE.key", "ﬁrebase-adminsdk.json")
	profiles := []fsfilter.Profile{{}, {Normalize: true}, {Fold: true}, {Normalize: true, Fold: true}}
	hidden := 0
	for _, name := range names {
		if Hidden(name) {
			hidden++
		}
		for _, profile := range profiles {
			form := fsfilter.NormalizeName(name, profile)
			if got, want := walk(form), Hidden(form); got != want {
				t.Errorf("table says %q hidden=%t, Hidden says %t (%q under %+v)", form, got, want, name, profile)
			}
		}
	}
	if hidden < 1000 || hidden == len(names) {
		t.Fatalf("%d of %d names hidden: the corpus no longer exercises the globs", hidden, len(names))
	}
	// Every spelling a volume may treat as one secret name reaches the table hidden, whatever the
	// volume compares: another letter case, the Kelvin sign, a composed or a decomposed accent.
	for _, name := range []string{"K.PEM", "K.PEM", "Kelvin.key", "clé.pem", "clé.pem"} {
		for _, profile := range profiles {
			if form := fsfilter.NormalizeName(name, profile); !walk(form) {
				t.Errorf("%q under %+v is %q, which the table does not hide", name, profile, form)
			}
		}
	}
}

func TestCHeaderIsStableAndBounded(t *testing.T) {
	first, err := CHeader()
	if err != nil {
		t.Fatal(err)
	}
	second, err := CHeader()
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("header differs between builds: %v", err)
	}
	table, err := buildTable()
	if err != nil {
		t.Fatal(err)
	}
	if len(table.next) < 2 || len(table.next) > maxStates || len(table.hidden) != len(table.next) {
		t.Fatalf("table has %d states", len(table.next))
	}
	for q, row := range table.next {
		for col, n := range row {
			if int(n) >= len(table.next) {
				t.Fatalf("state %d column %d leads to missing state %d", q, col, n)
			}
		}
	}
	if rows := bytes.Count(first, []byte("},\n")); rows != len(table.next) {
		t.Fatalf("header has %d rows for %d states", rows, len(table.next))
	}
	t.Logf("%d states, %d bytes of kernel table", len(table.next), len(table.next)*(columns*2+1))
}

func TestTokensRefuseSyntaxTheTableCannotRead(t *testing.T) {
	for _, glob := range []string{"[ab].pem", `\*.pem`, "a/b", "Key.pem", "clé"} {
		if _, err := tokens(glob); err == nil {
			t.Errorf("tokens(%q) accepted", glob)
		}
	}
}
