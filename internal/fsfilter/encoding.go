package fsfilter

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"path"
	"slices"
)

// WriteKernelPolicies is the private, versioned wire format consumed by the bundled
// libbpf helper. It contains policy metadata only, never protected file contents.
func WriteKernelPolicies(w io.Writer, policies []Policy, unicodeData []byte) error {
	if len(policies) == 0 || len(policies) > 32 {
		return errors.New("invalid filesystem policy roots")
	}
	paths := map[PathKey]uint32{}
	entries := 0
	grants := 0
	plans := make([]map[string]uint32, len(policies))
	for i, policy := range policies {
		compiled, err := policy.Compile(uint32(i + 1))
		if err != nil {
			return err
		}
		for key, flags := range compiled {
			paths[key] = flags
		}
		plan := map[string]uint32{}
		ancestors := func(name string) {
			for p := path.Dir(name); p != "."; p = path.Dir(p) {
				plan[p] |= Custody
			}
		}
		for _, entry := range policy.Entries {
			plan[entry.Path] |= entry.Flags
			ancestors(entry.Path)
		}
		grants += len(policy.Grants)
		for _, path := range policy.Grants {
			if _, err := (Policy{Entries: []Entry{{Path: path, Flags: ReadOnly}}}).Compile(1); err != nil {
				return err
			}
			ancestors(path)
		}
		plans[i] = plan
		entries += len(plan)
	}
	if len(paths) > MaxPaths || len(policies)+entries+grants > MaxPaths {
		return errors.New("filesystem policy exceeds admission bounds")
	}
	// The bundled image owns this table. Reject truncated or unbounded input
	// before emitting a header; the loader validates each mapping as it seeds.
	if len(unicodeData) == 0 || len(unicodeData)%48 != 0 || len(unicodeData)/48 > 20000 {
		return errors.New("invalid filesystem Unicode table")
	}
	out := bufio.NewWriter(w)
	if _, err := out.WriteString("COOPFS03"); err != nil {
		return err
	}
	write := func(v any) error { return binary.Write(out, binary.LittleEndian, v) }
	if err := write([5]uint32{uint32(len(policies)), uint32(len(paths)), uint32(len(unicodeData) / 48), uint32(entries), uint32(grants)}); err != nil {
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
	if _, err := out.Write(unicodeData); err != nil {
		return err
	}
	for i, plan := range plans {
		names := make([]string, 0, len(plan))
		for name := range plan {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if err := write([3]uint32{uint32(i + 1), uint32(len(name)), plan[name]}); err != nil {
				return err
			}
			if _, err := out.WriteString(name); err != nil {
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
