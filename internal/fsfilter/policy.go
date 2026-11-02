// Package fsfilter compiles admitted repository paths for the cgroup filesystem guard.
// The caller owns policy discovery; this package never invents visibility rules.
package fsfilter

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	Secret uint32 = 1 << iota
	ReadOnly
	Custody
	Root
)

const (
	MaxDepth = 128
	MaxName  = 255
	MaxPaths = 100000
)

// Profile describes lookup equivalences established for an admitted share.
// Normalization and case sensitivity are independent filesystem properties.
type Profile struct {
	Normalize bool `json:"normalize"`
	Fold      bool `json:"fold"`
}

type Entry struct {
	Path  string `json:"path"`
	Flags uint32 `json:"flags"`
}

type Policy struct {
	Profile Profile `json:"profile"`
	// Entries contain the complete admitted protected inventory, including
	// descendants of hidden directories and existing hardlink aliases. The
	// loader pins this inventory without discovering additional host policy.
	Entries []Entry `json:"entries"`
	// Grants are reviewed read-only regular files. Only held object identities
	// grant reads; hashed paths never carry allow authority.
	Grants []string `json:"grants,omitempty"`
}

// PathKey is a denial-set fingerprint, not an authority or an allow capability.
// Collisions merge restrictions with OR, so they can only deny additional access.
type PathKey struct {
	A, B           uint64
	Root, Reserved uint32
}

type NameKey struct{ A, B uint64 }

const (
	nameSeedA  uint64 = 14695981039346656037
	nameSeedB  uint64 = 7809847782465536322
	namePrimeA uint64 = 1099511628211
	namePrimeB uint64 = 14029467366897019727
)

func NormalizeName(name string, profile Profile) string {
	if profile.Normalize {
		name = canonicalDecomposition(name)
	}
	if profile.Fold {
		name = cases.Fold().String(name)
	}
	if profile.Normalize {
		name = canonicalDecomposition(name)
	}
	return name
}

// x/text's whole-string normalizer inserts CGJ after 30 nonstarters for stream
// safety. Filesystem lookup needs canonical equivalence without added characters.
func canonicalDecomposition(name string) string {
	var out []rune
	var classes []uint8
	for _, r := range name {
		for _, d := range norm.NFD.String(string(r)) {
			class := norm.NFD.PropertiesString(string(d)).CCC()
			at := len(out)
			out = append(out, d)
			classes = append(classes, class)
			for class != 0 && at > 0 && classes[at-1] > class {
				out[at], classes[at] = out[at-1], classes[at-1]
				at--
			}
			out[at], classes[at] = d, class
		}
	}
	return string(out)
}

func NameFingerprint(name string, profile Profile) NameKey {
	key := NameKey{nameSeedA, nameSeedB}
	for _, r := range NormalizeName(name, profile) {
		key.A = (key.A ^ uint64(r)) * namePrimeA
		key.B = (key.B ^ uint64(r)) * namePrimeB
	}
	return key
}

func appendName(key PathKey, name NameKey) PathKey {
	key.A = key.A*namePrimeA ^ name.A
	key.B = key.B*namePrimeB ^ name.B
	return key
}

func (p Policy) Compile(root uint32) (map[PathKey]uint32, error) {
	if root == 0 || len(p.Entries) > MaxPaths {
		return nil, errors.New("filesystem policy exceeds its admission bounds")
	}
	compiled := make(map[PathKey]uint32)
	for _, entry := range p.Entries {
		if len(entry.Path) >= 4096 || entry.Path == "." || entry.Path != path.Clean(entry.Path) || strings.HasPrefix(entry.Path, "/") || entry.Path == ".." || strings.HasPrefix(entry.Path, "../") || entry.Flags == 0 || entry.Flags & ^(Secret|ReadOnly) != 0 {
			return nil, fmt.Errorf("invalid filesystem policy entry %q", entry.Path)
		}
		parts := strings.Split(entry.Path, "/")
		if len(parts) >= MaxDepth {
			return nil, fmt.Errorf("filesystem policy path exceeds %d components: %s", MaxDepth-1, entry.Path)
		}
		key := PathKey{A: nameSeedA, B: nameSeedB, Root: root}
		for i, part := range parts {
			if len(part) > MaxName || !utf8.ValidString(part) || strings.IndexByte(part, 0) >= 0 {
				return nil, fmt.Errorf("unsupported filesystem name in %q", entry.Path)
			}
			key = appendName(key, NameFingerprint(part, p.Profile))
			flags := uint32(Custody)
			if i == len(parts)-1 {
				flags = entry.Flags
			}
			compiled[key] |= flags
			if len(compiled) > MaxPaths {
				return nil, errors.New("filesystem policy and ancestors exceed admission bounds")
			}
		}
	}
	return compiled, nil
}
