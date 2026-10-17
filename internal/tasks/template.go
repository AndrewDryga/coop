package tasks

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// A queue without a declared task.md Template block retains the built-in starter.
// Once declared, the block is authoritative and malformed content fails loudly.
const fallbackTaskTemplate = `---
id: 2026-06-26-<slug>
title: <one-line outcome>
labels: []
updated: <ISO-8601 timestamp>
---

# <one-line outcome>

**Context:** <the problem, why it matters, and where it happens>

**Acceptance criteria:** <the result and checks that prove the work is finished>

**Approach:** <the steps to take; use spec.md for a longer plan>

## Subtasks

- [ ] <a small step with a way to check it worked>
`

type taskTemplate struct{ lines []string }

// taskTemplateLimit bounds the queue README.md a task template is read from.
const taskTemplateLimit = 256 << 10

// loadTaskTemplate reads the template block of README.md in queue, the queue folder, through
// readTaskTemplate. A queue without a README.md uses the built-in template.
func loadTaskTemplate(queue *os.Root) (taskTemplate, error) {
	path := filepath.Join(queue.Name(), "README.md")
	data, err := readTaskTemplate(queue, path)
	if errors.Is(err, os.ErrNotExist) {
		return parseTaskTemplate(fallbackTaskTemplate)
	}
	if err != nil {
		return taskTemplate{}, err
	}
	if len(data) > taskTemplateLimit || !utf8.Valid(data) {
		return taskTemplate{}, fmt.Errorf("task template %s: README is too large or not UTF-8", path)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	inTask, inTemplate := false, false
	var block []string
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			if inTemplate {
				break
			}
			inTask = strings.HasPrefix(line, "## task.md")
		}
		if !inTask {
			continue
		}
		if !inTemplate {
			inTemplate = strings.TrimSpace(line) == "**Template:**"
			continue
		}
		if strings.HasPrefix(line, "    ") {
			block = append(block, strings.TrimPrefix(line, "    "))
			continue
		}
		if strings.TrimSpace(line) != "" {
			break
		}
		block = append(block, "")
	}
	if !inTemplate {
		return parseTaskTemplate(fallbackTaskTemplate)
	}
	for len(block) > 0 && block[0] == "" {
		block = block[1:]
	}
	for len(block) > 0 && block[len(block)-1] == "" {
		block = block[:len(block)-1]
	}
	t, err := parseTaskTemplate(strings.Join(block, "\n") + "\n")
	if err != nil {
		return taskTemplate{}, fmt.Errorf("task template %s: %w", path, err)
	}
	return t, nil
}

// readTaskTemplate reads README.md in queue, found at path, without following a symbolic link or
// waiting on a FIFO, and at most one byte past taskTemplateLimit. A symbolic link is refused: it
// could point task creation at a file outside the queue. Other hard links are fine, unlike for a
// task file: the template is only read, and a box can hard-link only files inside its own mount.
func readTaskTemplate(queue *os.Root, path string) ([]byte, error) {
	before, err := queue.Lstat("README.md")
	if err != nil {
		return nil, fmt.Errorf("read task template %s: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("task template %s is a symbolic link — replace the link with a copy of the file, or delete it to use the built-in template", path)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("task template %s is not a regular file", path)
	}
	f, err := queue.OpenFile("README.md", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("read task template %s: %w", path, err)
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read task template %s: %w", path, err)
	}
	// os.Root follows a link inside it even with O_NOFOLLOW, so the opened file must be the one
	// checked above.
	if !os.SameFile(before, after) {
		return nil, fmt.Errorf("task template %s changed while it was opened", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, taskTemplateLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read task template %s: %w", path, err)
	}
	return data, nil
}

func parseTaskTemplate(src string) (taskTemplate, error) {
	lines := strings.Split(strings.TrimSuffix(src, "\n"), "\n")
	if len(lines) < 4 || lines[0] != "---" {
		return taskTemplate{}, errors.New("task.md Template needs a frontmatter block")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return taskTemplate{}, errors.New("task.md Template has no closing frontmatter fence")
	}
	for _, key := range []string{"id", "title", "updated"} {
		count := 0
		for _, line := range lines[1:end] {
			if strings.HasPrefix(line, key+":") {
				count++
			}
		}
		if count != 1 {
			return taskTemplate{}, fmt.Errorf("task.md Template needs exactly one %s: field", key)
		}
	}
	h1, checklist, placeholder, subtasks := 0, 0, 0, false
	for _, line := range lines[end+1:] {
		if strings.HasPrefix(line, "# ") {
			h1++
		}
		if line == "## Subtasks" {
			subtasks = true
			continue
		}
		if strings.HasPrefix(line, "## ") {
			subtasks = false
		}
		if subtasks && strings.HasPrefix(line, "- [ ] ") {
			checklist++
			if isTaskStepPlaceholder(line) {
				placeholder++
			}
		}
	}
	if h1 != 1 || checklist == 0 || placeholder > 1 {
		return taskTemplate{}, errors.New("task.md Template needs one # title, a nonempty ## Subtasks checklist, and at most one <…> step placeholder")
	}
	for _, s := range taskSections {
		count := 0
		for _, line := range lines[end+1:] {
			if strings.HasPrefix(line, "**"+s.heading+":**") {
				count++
			}
		}
		if count != 1 {
			return taskTemplate{}, fmt.Errorf("task.md Template needs exactly one **%s:** section", s.heading)
		}
	}
	return taskTemplate{lines: lines}, nil
}

func isTaskStepPlaceholder(line string) bool {
	v := strings.TrimPrefix(line, "- [ ] ")
	return strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">")
}

func taskTitleScalar(title string) string {
	switch strings.ToLower(title) {
	case "true", "false", "null", "yes", "no", "on", "off":
		return strconv.Quote(title)
	}
	for i, r := range title {
		if i == 0 && !unicode.IsLetter(r) {
			return strconv.Quote(title)
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != ' ' && r != '-' && r != '_' && r != '.' && r != '/' {
			return strconv.Quote(title)
		}
	}
	return title
}

// checkTaskText refuses a title or subtask that is not one safe line within its limit. It does not
// depend on the template, so `add` runs it before it makes a missing queue folder.
func checkTaskText(title string, subtasks []string) error {
	if !ValidTaskText(title, false, TaskTitleLimit) {
		return fmt.Errorf("task title must be one safe line of at most %d bytes", TaskTitleLimit)
	}
	for _, step := range subtasks {
		if !ValidTaskText(step, false, TaskLineLimit) {
			return fmt.Errorf("subtask must be one safe line of at most %d bytes", TaskLineLimit)
		}
	}
	return nil
}

func (t taskTemplate) render(id, title, now string, values map[string]string, subtasks []string) (string, error) {
	if err := checkTaskText(title, subtasks); err != nil {
		return "", err
	}
	var out []string
	inSubtasks := false
	frontmatterEnd := 0
	for i := 1; i < len(t.lines); i++ {
		if t.lines[i] == "---" {
			frontmatterEnd = i
			break
		}
	}
	inserted := false
	for i, line := range t.lines {
		switch {
		case i > 0 && i < frontmatterEnd && strings.HasPrefix(line, "id:"):
			line = "id: " + id
		case i > 0 && i < frontmatterEnd && strings.HasPrefix(line, "title:"):
			line = "title: " + taskTitleScalar(title)
		case i > 0 && i < frontmatterEnd && strings.HasPrefix(line, "updated:"):
			line = "updated: " + now
		case i > frontmatterEnd && strings.HasPrefix(line, "# "):
			line = "# " + title
		case line == "## Subtasks":
			inSubtasks = true
		case strings.HasPrefix(line, "## "):
			if inSubtasks && !inserted && len(subtasks) > 0 {
				for _, step := range subtasks {
					out = append(out, "- [ ] "+step)
				}
				inserted = true
			}
			inSubtasks = false
		}
		for _, s := range taskSections {
			if v := strings.TrimSpace(values[s.heading]); v != "" && strings.HasPrefix(line, "**"+s.heading+":**") {
				line = "**" + s.heading + ":** " + v
			}
		}
		if inSubtasks && isTaskStepPlaceholder(line) && len(subtasks) > 0 {
			for _, step := range subtasks {
				out = append(out, "- [ ] "+step)
			}
			inserted = true
			continue
		}
		out = append(out, line)
	}
	if len(subtasks) > 0 && !inserted {
		for _, step := range subtasks {
			out = append(out, "- [ ] "+step)
		}
	}
	return strings.Join(out, "\n") + "\n", nil
}
