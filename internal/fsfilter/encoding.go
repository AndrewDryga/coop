package fsfilter

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"slices"
)

// WriteKernelPolicy is the private, versioned wire format consumed by the bundled
// libbpf helper. It contains policy metadata only, never protected file contents.
func WriteKernelPolicy(w io.Writer, policy Policy) error {
	return WriteKernelPolicies(w, []Policy{policy})
}

func WriteKernelPolicies(w io.Writer, policies []Policy) error {
	if len(policies) == 0 || len(policies) > 32 {
		return errors.New("invalid filesystem policy roots")
	}
	paths := map[PathKey]uint32{}
	entries := 0
	grants := 0
	for i, policy := range policies {
		compiled, err := policy.Compile(uint32(i + 1))
		if err != nil {
			return err
		}
		for key, flags := range compiled {
			paths[key] = flags
		}
		entries += len(policy.Entries)
		grants += len(policy.Grants)
		for _, path := range policy.Grants {
			if _, err := (Policy{Entries: []Entry{{Path: path, Flags: ReadOnly}}}).Compile(1); err != nil {
				return err
			}
		}
	}
	if len(paths) > MaxPaths || entries+grants > MaxPaths {
		return errors.New("filesystem policy exceeds admission bounds")
	}
	table, err := UnicodeTable()
	if err != nil {
		return err
	}
	out := bufio.NewWriter(w)
	if _, err := out.WriteString("COOPFS02"); err != nil {
		return err
	}
	write := func(v any) error { return binary.Write(out, binary.LittleEndian, v) }
	if err := write([5]uint32{uint32(len(policies)), uint32(len(paths)), uint32(len(table)), uint32(entries), uint32(grants)}); err != nil {
		return err
	}
	for i, policy := range policies {
		flags := uint32(0)
		if policy.Profile.Normalize {
			flags |= 1
		}
		if policy.Profile.Fold {
			flags |= 2
		}
		if err := write([2]uint32{uint32(i + 1), flags}); err != nil {
			return err
		}
	}
	keys := make([]PathKey, 0, len(paths))
	for key := range paths {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b PathKey) int {
		if a.Root < b.Root {
			return -1
		}
		if a.Root > b.Root {
			return 1
		}
		if a.A < b.A || a.A == b.A && a.B < b.B {
			return -1
		}
		if a == b {
			return 0
		}
		return 1
	})
	for _, key := range keys {
		if err := write(key); err != nil {
			return err
		}
		if err := write(paths[key]); err != nil {
			return err
		}
	}
	runes := make([]uint32, 0, len(table))
	for rune := range table {
		runes = append(runes, rune)
	}
	slices.Sort(runes)
	for _, rune := range runes {
		if err := write(rune); err != nil {
			return err
		}
		if err := write(table[rune]); err != nil {
			return err
		}
	}
	for i, policy := range policies {
		for _, entry := range policy.Entries {
			if err := write([3]uint32{uint32(i + 1), uint32(len(entry.Path)), entry.Flags}); err != nil {
				return err
			}
			if _, err := out.WriteString(entry.Path); err != nil {
				return err
			}
		}
	}
	for i, policy := range policies {
		for _, path := range policy.Grants {
			if err := write([3]uint32{uint32(i + 1), uint32(len(path)), ReadOnly | 16}); err != nil {
				return err
			}
			if _, err := out.WriteString(path); err != nil {
				return err
			}
		}
	}
	return out.Flush()
}
