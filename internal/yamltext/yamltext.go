// Package yamltext rewords YAML parser errors for the person editing the file.
package yamltext

import (
	"errors"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

var (
	yamlUnknownField = regexp.MustCompile(`field (.+) not found in type \S+$`)
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
