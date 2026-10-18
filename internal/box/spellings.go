package box

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A Mac's disk ignores letter case, so a box that opens .ENV reads the .env a decoy covers under
// one spelling. Coop covers each other spelling too, 2^letters covers per name. Measured on
// OrbStack: 2,048 covers start a box in 0.6 s, 8,192 in 2.3 s, and 32,768 overflow the docker
// command line.
const (
	// maxCoveredLetters bounds the letters of a name Coop covers under every spelling: eleven fully
	// hide .env, .envrc, id_rsa, .env.local, database.yml and .coopignore.
	maxCoveredLetters = 11
	// maxCoverBytes bounds the docker command-line bytes one launch's covers take: macOS passes at
	// most 1 MiB of arguments and environment to a program.
	maxCoverBytes = 640 << 10
	// coverOverhead is a cover's bytes beside its target: the -v, the decoy or policy snapshot path
	// it binds, and ":ro".
	coverOverhead = 128
)

// UncoveredSpellings is the refusal of hidden files whose every letter-case spelling Coop cannot
// cover on a disk that ignores case, so a box could open one under another spelling and read the
// real file. ComputeMounts returns it together with the mounts, so a launch whose owner accepted
// that risk (COOP_ALLOW_CASE_EXPOSED_SECRETS) can still start.
type UncoveredSpellings struct {
	Paths []string // repo-relative, slash-separated
}

func (e *UncoveredSpellings) Error() string {
	return "a box could read " + strings.Join(e.Paths, ", ") + " under another letter case: Coop cannot cover every spelling"
}

// spellingBudget is one launch's covers so far.
type spellingBudget struct{ bytes int }

// covers returns the other spellings of the hidden file at p, mounted at target, that need a cover:
// none on a disk that keeps letter case, every one when they fit the launch's budget, or false when
// they cannot all be covered.
func (b *spellingBudget) covers(p, target string) ([]string, bool) {
	if !foldsCase(p) {
		return nil, true
	}
	spellings, ok := otherSpellings(filepath.Base(p))
	if !ok {
		return nil, false
	}
	cost := len(spellings) * (len(path.Dir(target)) + 1 + len(filepath.Base(p)) + coverOverhead)
	if b.bytes+cost > maxCoverBytes {
		return nil, false
	}
	b.bytes += cost
	return spellings, true
}

// foldsCase reports whether the disk opens path under another spelling of its name: true on a
// case-insensitive Mac disk, false on Linux or a case-sensitive volume. A name without letters has
// no other spelling.
func foldsCase(path string) bool {
	name := filepath.Base(path)
	swapped := swapCase(name)
	if swapped == name {
		return false
	}
	want, err := os.Lstat(path)
	if err != nil {
		return false
	}
	got, err := os.Lstat(filepath.Join(filepath.Dir(path), swapped))
	return err == nil && os.SameFile(want, got)
}

// otherSpellings returns every other letter-case spelling of name, or false when there are too
// many to cover: more than maxCoveredLetters ASCII letters, or any non-ASCII letter, whose
// spellings a Mac disk folds by Unicode rules this does not enumerate.
func otherSpellings(name string) ([]string, bool) {
	var letters []int
	for i, r := range name {
		if r >= utf8.RuneSelf {
			if unicode.IsLetter(r) {
				return nil, false
			}
			continue
		}
		if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' {
			letters = append(letters, i)
		}
	}
	if len(letters) > maxCoveredLetters {
		return nil, false
	}
	spellings := make([]string, 0, 1<<len(letters)-1)
	for mask := 0; mask < 1<<len(letters); mask++ {
		spelled := []byte(name)
		for bit, i := range letters {
			if mask&(1<<bit) != 0 {
				spelled[i] = byte(unicode.ToUpper(rune(spelled[i])))
			} else {
				spelled[i] = byte(unicode.ToLower(rune(spelled[i])))
			}
		}
		if string(spelled) != name {
			spellings = append(spellings, string(spelled))
		}
	}
	return spellings, true
}

func swapCase(name string) string {
	swapped := []byte(name)
	for i, c := range swapped {
		switch {
		case 'a' <= c && c <= 'z':
			swapped[i] = c - 'a' + 'A'
		case 'A' <= c && c <= 'Z':
			swapped[i] = c - 'A' + 'a'
		}
	}
	return string(swapped)
}
