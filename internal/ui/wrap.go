package ui

import "strings"

// WrapLines wraps plain display text to width before callers add ANSI styling. Long tokens are
// split rather than left to the terminal, where an implicit wrap can corrupt a live region.
func WrapLines(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	line := ""
	flush := func() {
		if line != "" {
			lines = append(lines, line)
			line = ""
		}
	}
	for _, word := range words {
		for len([]rune(word)) > width {
			flush()
			runes := []rune(word)
			lines = append(lines, string(runes[:width]))
			word = string(runes[width:])
		}
		if line == "" {
			line = word
			continue
		}
		if len([]rune(line))+1+len([]rune(word)) <= width {
			line += " " + word
			continue
		}
		flush()
		line = word
	}
	flush()
	return lines
}

// PrefixedLines wraps a value under its label. A long indivisible value gets its own indented
// row, keeping canonical targets and commands readable rather than squeezing them after a label.
func PrefixedLines(prefix, value string, width int) []string {
	if width <= len([]rune(prefix)) {
		return WrapLines(prefix+value, width)
	}
	if !strings.ContainsAny(value, " \t\n") && len([]rune(value)) > width-len([]rune(prefix)) {
		lines := []string{strings.TrimSpace(prefix)}
		for _, line := range WrapLines(value, width-2) {
			lines = append(lines, "  "+line)
		}
		return lines
	}
	wrapped := WrapLines(value, width-len([]rune(prefix)))
	for i := range wrapped {
		if i == 0 {
			wrapped[i] = prefix + wrapped[i]
		} else {
			wrapped[i] = strings.Repeat(" ", len([]rune(prefix))) + wrapped[i]
		}
	}
	return wrapped
}
