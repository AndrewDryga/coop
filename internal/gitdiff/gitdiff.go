// Package gitdiff decodes Git's exact-path, NUL-delimited diff formats.
package gitdiff

import (
	"fmt"
	"strconv"
	"strings"
)

type Change struct {
	Status  string
	OldPath string // populated for renames and copies
	Path    string
}

// NameStatus reads --name-status -z output without trimming or unquoting paths.
func NameStatus(data string) ([]Change, error) {
	var changes []Change
	for data != "" {
		status, rest, err := field(data)
		if err != nil {
			return nil, err
		}
		if !strings.ContainsRune("ACDMRTUXB", rune(status[0])) {
			return nil, fmt.Errorf("invalid Git change status %q", status)
		}
		if len(status) > 1 {
			score, err := strconv.Atoi(status[1:])
			if !strings.ContainsRune("MRC", rune(status[0])) || err != nil || score < 0 || score > 100 {
				return nil, fmt.Errorf("invalid Git change status %q", status)
			}
		}
		path, rest, err := field(rest)
		if err != nil {
			return nil, err
		}
		change := Change{Status: status, Path: path}
		if status[0] == 'R' || status[0] == 'C' {
			change.OldPath = path
			change.Path, rest, err = field(rest)
			if err != nil {
				return nil, err
			}
		}
		changes = append(changes, change)
		data = rest
	}
	return changes, nil
}

// Numstat reads --numstat -z counts keyed by the destination path. Binary counts
// stay unknown (-1); rename/copy records carry separate old and new path fields.
func Numstat(data string) (map[string][2]int, error) {
	counts := make(map[string][2]int)
	for data != "" {
		record, rest, err := field(data)
		if err != nil {
			return nil, err
		}
		parts := strings.SplitN(record, "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid Git numstat record")
		}
		path := parts[2]
		if path == "" {
			_, rest, err = field(rest) // source path
			if err != nil {
				return nil, err
			}
			path, rest, err = field(rest)
			if err != nil {
				return nil, err
			}
		}
		pair := [2]int{-1, -1}
		if parts[0] != "-" || parts[1] != "-" {
			for i := range pair {
				pair[i], err = strconv.Atoi(parts[i])
				if err != nil || pair[i] < 0 {
					return nil, fmt.Errorf("invalid Git numstat count %q", parts[i])
				}
			}
		}
		counts[path] = pair
		data = rest
	}
	return counts, nil
}

func field(data string) (string, string, error) {
	value, rest, ok := strings.Cut(data, "\x00")
	if !ok || value == "" {
		return "", "", fmt.Errorf("incomplete Git diff record")
	}
	return value, rest, nil
}
