package box

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// captureRequiredMCPEnv freezes only the native renderer's bearer references, after runtime
// options are assembled. Docker applies every env file before explicit -e assignments, regardless
// of their relative positions. Pin the resulting values in a final private env file and remove
// those keys' -e overrides: no secret moves into argv, and later host-file edits cannot silently
// turn authenticated MCP into anonymous access. Other environment behavior remains unchanged.
func captureRequiredMCPEnv(options, required []string, artifacts compositionArtifactOps) ([]string, string, error) {
	if len(required) == 0 {
		return options, "", nil
	}
	wanted := map[string]bool{}
	for _, key := range required {
		if key == "" || len(key) > 128 || strings.Trim(key, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" || strings.ContainsRune("0123456789", rune(key[0])) {
			return nil, "", errors.New("MCP authentication requires a valid environment variable name")
		}
		wanted[key] = true
	}
	values := map[string]string{}
	var assignments []string
	var kept []string
	for i := 0; i < len(options); i++ {
		start := i
		flag, value, inline := strings.Cut(options[i], "=")
		if strings.HasPrefix(options[i], "-e") && !strings.HasPrefix(options[i], "--") && options[i] != "-e" {
			flag, value, inline = "-e", strings.TrimPrefix(options[i], "-e"), true
		}
		switch flag {
		case "-e", "--env", "--env-file":
			if !inline {
				if i+1 == len(options) {
					return nil, "", errors.New("MCP authentication environment option is missing its value")
				}
				i++
				value = options[i]
			}
			if flag == "--env-file" {
				data, err := os.ReadFile(value)
				if err != nil {
					return nil, "", errors.New("MCP authentication environment file could not be read")
				}
				for key, value := range envFileValues(data) {
					if wanted[key] {
						values[key] = value
					}
				}
			} else if key, _, _ := strings.Cut(value, "="); wanted[key] {
				assignments = append(assignments, value)
				continue // the captured file replaces this exact key, including bare shell imports
			}
		case "-v", "--volume", "--mount", "-w", "--workdir", "--memory", "--cpus", "--pids-limit",
			"--cap-drop", "--cap-add", "--security-opt", "--network", "--user", "--log-driver",
			"--log-opt", "--tmpfs", "--entrypoint", "--label", "--name", "-p", "--publish", "--hostname":
			if !inline && i+1 < len(options) {
				i++ // an unrelated option's value is not an environment flag
			}
		}
		kept = append(kept, options[start:i+1]...)
	}
	for _, assignment := range assignments {
		key, value, explicit := strings.Cut(assignment, "=")
		if !explicit {
			value = os.Getenv(key) // only an explicit runtime -e KEY authorizes a shell import
		}
		values[key] = value
	}
	keys := make([]string, 0, len(wanted))
	for key := range wanted {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var content strings.Builder
	for _, key := range keys {
		value := values[key]
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			return nil, "", fmt.Errorf("MCP authentication requires a nonempty single-line %s in the selected box environment; configure it before retrying", key)
		}
		fmt.Fprintf(&content, "%s=%s\n", key, value)
	}
	file, err := artifacts.writeFile(artifacts.parent, content.String())
	if err != nil {
		return nil, "", errors.New("MCP authentication environment could not be captured")
	}
	return append(kept, "--env-file", file), file, nil
}
