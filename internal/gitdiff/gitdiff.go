// Package gitdiff decodes Git's exact-path, NUL-delimited diff formats, and the hunk headers of
// a one-file patch.
package gitdiff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// LineRanges are the new-file lines a patch adds, each run as its first and last line.
type LineRanges [][2]int

// Contains reports whether line n is one of the added lines.
func (r LineRanges) Contains(n int) bool {
	for _, run := range r {
		if run[0] <= n && n <= run[1] {
			return true
		}
	}
	return false
}

// hunkHeader is a two-way hunk header, "@@ -a[,b] +c[,d] @@", whose new side is line c and d lines.
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// AddedLines reads the patch `git diff -U0` writes for one file and returns the lines its hunks add.
// Without context lines every hunk's new side is added text, and a body line never starts with "@@".
func AddedLines(patch string) (LineRanges, error) {
	var added LineRanges
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		m := hunkHeader.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("invalid Git hunk header %q", line)
		}
		start, err := strconv.Atoi(m[1])
		count := 1
		if err == nil && m[2] != "" {
			count, err = strconv.Atoi(m[2])
		}
		if err != nil {
			return nil, fmt.Errorf("invalid Git hunk header %q", line)
		}
		if count > 0 {
			added = append(added, [2]int{start, start + count - 1})
		}
	}
	return added, nil
}

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
