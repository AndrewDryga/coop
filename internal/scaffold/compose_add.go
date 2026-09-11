package scaffold

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Adding a service to a project that already has one. This is an EDIT, not a rewrite: the file
// may carry a person's own services, their comments, their ports and their volumes, and none of
// that survives a YAML re-marshal. So the block is spliced in as text, exactly the way
// project.yaml's subproject registration works, and the result is parsed before it is written —
// a file coop cannot re-read is a file coop must not leave behind.
//
// Nothing here starts anything. Adding a service and running it are two decisions, and the second
// one belongs to the person typing `coop up`.

// ComposeAddition reports what an additive service edit did, so the caller can say it exactly.
type ComposeAddition struct {
	Rel      string   // the repo-relative Compose path the edit targeted
	Created  bool     // the file did not exist and was written whole
	Added    []string // catalog names added, in catalog order
	Existing []string // catalog names already configured, in catalog order
}

// ComposeCollision is a service name the catalog wants that the project already uses for
// something else. Coop will not rename or replace it: the name is the project's.
type ComposeCollision struct {
	Catalog string // the catalog service that could not be added
	Name    string // the Compose service name already taken
}

func (e *ComposeCollision) Error() string {
	return fmt.Sprintf("the name %s is already used by a different service", e.Name)
}

// ComposeParseError is a Compose file coop could not read. The line is the parser's, so the fix
// is an edit at a place the person can find.
type ComposeParseError struct {
	Line    int
	Problem string
}

func (e *ComposeParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("YAML line %d: %s", e.Line, e.Problem)
	}
	return e.Problem
}

// ServiceName is the Compose service a catalog entry writes — "postgres" writes a service called
// "db" — so a caller can report the mapping a person will see in their file.
func ServiceName(catalog string) string { return composeCatalog[catalog].service }

// AddComposeServices adds the named catalog services to rel (repo-relative) without disturbing
// anything already in it. An absent file is created from the catalog; an existing one is edited in
// place. On any failure the file on disk is left exactly as it was.
func AddComposeServices(repo, rel string, services []string) (ComposeAddition, error) {
	out := ComposeAddition{Rel: rel}
	path := filepath.Join(repo, filepath.FromSlash(rel))
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		content := composeFor(services)
		if content == "" {
			return out, nil
		}
		if err := (&scaffolder{repo: repo}).writeContentIfAbsent(path, content, 0o644); err != nil {
			return out, err
		}
		out.Created = true
		for _, name := range ComposeServices {
			if slices.Contains(services, name) {
				out.Added = append(out.Added, name)
			}
		}
		return out, nil
	}
	if err != nil {
		return out, err
	}
	existing, err := composeServiceImages(data)
	if err != nil {
		return out, err
	}
	text := string(data)
	for _, name := range ComposeServices {
		if !slices.Contains(services, name) {
			continue
		}
		unit := composeCatalog[name]
		image, taken := existing[unit.service]
		switch {
		case taken && strings.HasPrefix(image, unit.image):
			// The same service under the same name: already configured, and its data stays put.
			out.Existing = append(out.Existing, name)
			continue
		case taken:
			return ComposeAddition{Rel: rel}, &ComposeCollision{Catalog: name, Name: unit.service}
		}
		text = spliceService(text, unit)
		out.Added = append(out.Added, name)
	}
	if len(out.Added) == 0 {
		return out, nil
	}
	// Re-read what the edit produced before it reaches disk: an unparseable Compose file would
	// break `coop up` for a project that was working a moment ago.
	if _, err := composeServiceImages([]byte(text)); err != nil {
		return ComposeAddition{Rel: rel}, err
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return ComposeAddition{Rel: rel}, err
	}
	return out, nil
}

// spliceService inserts one service block at the end of the services mapping and declares its
// volume, leaving every existing line — comments included — byte for byte where it was.
func spliceService(text string, unit composeUnit) string {
	lines := strings.Split(text, "\n")
	insert := func(at int, block []string) {
		lines = append(lines[:at], append(block, lines[at:]...)...)
	}
	if at, ok := blockEnd(lines, "services:"); ok {
		insert(at, strings.Split(strings.TrimRight(unit.block, "\n"), "\n"))
	} else {
		lines = append(lines, "services:")
		lines = append(lines, strings.Split(strings.TrimRight(unit.block, "\n"), "\n")...)
	}
	if unit.volume != "" {
		if at, ok := blockEnd(lines, "volumes:"); ok {
			insert(at, []string{"  " + unit.volume + ":"})
		} else {
			lines = append(lines, "volumes:", "  "+unit.volume+":")
		}
	}
	joined := strings.Join(lines, "\n")
	if !strings.HasSuffix(joined, "\n") {
		joined += "\n"
	}
	return joined
}

// blockEnd finds the index just past the last line of a top-level mapping, skipping the blank and
// comment lines that trail it — so an inserted entry lands inside the block rather than after a
// comment that introduces the next one.
func blockEnd(lines []string, key string) (int, bool) {
	start := -1
	for i, line := range lines {
		if strings.TrimRight(line, " \t") == key {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, false
	}
	end := start + 1
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue // may belong to this block or the next; the last indented line decides
		}
		if !strings.HasPrefix(lines[i], " ") && !strings.HasPrefix(lines[i], "\t") {
			break // a new top-level key ends the block
		}
		end = i + 1
	}
	return end, true
}

// composeServiceImages maps each declared service to the image it runs, and is also the file's
// parse check: a Compose file coop cannot read is reported with the line the parser stopped on
// rather than silently treated as having no services.
func composeServiceImages(data []byte) (map[string]string, error) {
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		line, problem := yamlProblem(err)
		return nil, &ComposeParseError{Line: line, Problem: problem}
	}
	images := map[string]string{}
	for name, service := range doc.Services {
		images[name] = service.Image
	}
	return images, nil
}

// yamlProblem pulls the line number and message out of a yaml error, whose text is
// "yaml: line N: <problem>" or a type-error list.
func yamlProblem(err error) (int, string) {
	msg := err.Error()
	if typed, ok := err.(*yaml.TypeError); ok && len(typed.Errors) > 0 {
		msg = typed.Errors[0]
	}
	msg = strings.TrimPrefix(msg, "yaml: ")
	line := 0
	if rest, ok := strings.CutPrefix(msg, "line "); ok {
		if number, tail, found := strings.Cut(rest, ":"); found {
			if _, scanErr := fmt.Sscanf(number, "%d", &line); scanErr == nil {
				msg = strings.TrimSpace(tail)
			}
		}
	}
	return line, msg
}
