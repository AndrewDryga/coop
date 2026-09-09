package networkstate

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
)

// Qualifications returns host-owned completed records, newest first. It reads
// bounded directory batches and refuses an oversized result instead of hiding
// candidates. It performs no probes, canaries or runtime mutations.
func (s *Store) Qualifications(ctx context.Context) ([]Qualification, error) {
	if err := s.intactAuthority(); err != nil {
		return nil, err
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	var result []Qualification
	var scanned int
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		names, err := dir.Readdirnames(256)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		scanned += len(names)
		if scanned > 65536 {
			return nil, errors.New("network qualification index exceeds its directory scan limit")
		}
		for _, name := range names {
			if !strings.HasPrefix(name, "qualification-") || !strings.HasSuffix(name, ".json") {
				continue
			}
			id := strings.TrimSuffix(strings.TrimPrefix(name, "qualification-"), ".json")
			if !lowerHex(id, 64) {
				continue
			}
			q, err := s.Qualification(id)
			if errors.Is(err, errObsoleteQualification) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if len(result) >= 256 {
				return nil, errors.New("network qualification index exceeds its retained record limit")
			}
			result = append(result, q)
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	slices.SortFunc(result, func(a, b Qualification) int { return b.CompletedAt.Compare(a.CompletedAt) })
	return result, nil
}
