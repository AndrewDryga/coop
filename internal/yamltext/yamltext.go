// Package yamltext rewords YAML parser errors for the person editing the file.
package yamltext

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

var (
	yamlUnknownField = regexp.MustCompile(`(?s)field (.+) not found in type \S+$`)
	yamlWrongType    = regexp.MustCompile("cannot unmarshal !!\\w+ `(.*)` into (\\S+)$")
)

// Plain rewords yaml's type problems for the person editing the file: it names the setting
// and what it should be, not the Go type behind it. Other errors come back as they are.
func Plain(err error) error {
	var problems *yaml.TypeError
	if !errors.As(err, &problems) {
		return err
	}
	plain := make([]string, len(problems.Errors))
	for i, problem := range problems.Errors {
		problem = yamlUnknownField.ReplaceAllString(problem, `unknown setting "$1"`)
		if match := yamlWrongType.FindStringSubmatch(problem); match != nil {
			problem = strings.Replace(problem, match[0], `"`+match[1]+`" should be `+yamlExpected(match[2]), 1)
		}
		plain[i] = problem
	}
	return &yaml.TypeError{Errors: plain}
}

// InFile is Plain for a file the person edits, worded the way they look for it: each problem on a
// line of its own, after the file's name and with no "yaml:" header from the parser
// (`.agent/loop.yaml line 2: unknown setting "bogus"`). The yaml.TypeError stays underneath, so
// the CLI still escapes each problem exactly as written.
func InFile(name string, err error) error {
	var problems *yaml.TypeError
	if errors.As(Plain(err), &problems) {
		return &fileProblems{name: name, problems: problems}
	}
	if rest, ok := strings.CutPrefix(err.Error(), "yaml: "); ok {
		if strings.HasPrefix(rest, "line ") {
			return errors.New(name + " " + rest)
		}
		return errors.New(name + ": " + rest)
	}
	return fmt.Errorf("%s: %w", name, err)
}

type fileProblems struct {
	name     string
	problems *yaml.TypeError
}

func (e *fileProblems) Error() string {
	lines := make([]string, len(e.problems.Errors))
	for i, problem := range e.problems.Errors {
		lines[i] = e.name + " " + problem
	}
	return strings.Join(lines, "\n")
}

func (e *fileProblems) Unwrap() error { return e.problems }

func yamlExpected(goType string) string {
	switch {
	case strings.HasPrefix(goType, "[]"):
		return "a list"
	case strings.HasPrefix(goType, "int"), strings.HasPrefix(goType, "uint"), strings.HasPrefix(goType, "float"):
		return "a number"
	case goType == "bool":
		return "true or false"
	case goType == "string":
		return "text"
	default:
		return "a group of settings"
	}
}
