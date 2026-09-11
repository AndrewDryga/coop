package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/ui"
)

type networkFlags struct {
	Mode      *egress.Mode
	Domains   []string
	RulesFile string
}

// Coop flags end at its first separator. Mode presence is retained: the shared
// admission boundary, not the parser, resolves remembered posture and conflicts. command is the
// launch the person typed ("coop run", "coop fork"), so a refused value names the command it came
// from and points at that command's page — these flags belong to every launch, not to one of them.
func extractNetworkFlags(command string, args []string) (networkFlags, []string, error) {
	var flags networkFlags
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); {
		if args[i] == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		consumed := false
		for _, name := range []string{"--egress", "--allow-domain", "--egress-rules"} {
			value, count, match, err := flagValue(args, i, name)
			if !match {
				continue
			}
			if err != nil {
				return networkFlags{}, nil, ui.MissingOptionValue(name, command, networkFlagExample(command, name))
			}
			switch name {
			case "--egress":
				if flags.Mode != nil {
					return networkFlags{}, nil, ui.RepeatedOption("--egress", command)
				}
				mode, err := egress.ParseMode(value)
				if err != nil {
					return networkFlags{}, nil, ui.InvalidOptionValue(value, "--egress", command,
						"Choose filtered, open or none.", command+" --egress filtered")
				}
				flags.Mode = &mode
			case "--allow-domain":
				if len(flags.Domains) >= egress.MaxRules {
					return networkFlags{}, nil, ui.InvalidOptionValue(value, "--allow-domain", command,
						fmt.Sprintf("A run may allow at most %d hosts.", egress.MaxRules),
						"Put the rest in a file and pass "+command+" --egress-rules <file>")
				}
				domain, err := egress.NormalizeDomain(value, false)
				if err != nil {
					return networkFlags{}, nil, ui.InvalidOptionValue(value, "--allow-domain", command,
						upperFirstSentence(err.Error()), command+" --allow-domain docs.example.com")
				}
				flags.Domains = append(flags.Domains, domain)
			case "--egress-rules":
				if flags.RulesFile != "" {
					return networkFlags{}, nil, ui.RepeatedOption("--egress-rules", command)
				}
				flags.RulesFile = value
			}
			i += count
			consumed = true
			break
		}
		if !consumed {
			rest = append(rest, args[i])
			i++
		}
	}
	return flags, rest, nil
}

func (f networkFlags) admission() box.NetworkAdmission {
	return box.NetworkAdmission{InvocationMode: f.Mode, Domains: f.Domains, RulesFile: f.RulesFile}
}

func (f networkFlags) set() bool { return f.Mode != nil || len(f.Domains) != 0 || f.RulesFile != "" }

// args re-renders the flags in the exact spelling a re-executed worker parses, so
// a detached fork loop admits what its foreground twin would have. The rules file
// is absolutized: the worker starts in the parent repository, and a path that
// silently resolved somewhere else would freeze a policy nobody chose.
func (f networkFlags) args() ([]string, error) {
	var out []string
	if f.Mode != nil {
		out = append(out, "--egress", string(*f.Mode))
	}
	for _, domain := range f.Domains {
		out = append(out, "--allow-domain", domain)
	}
	if f.RulesFile != "" {
		path, err := filepath.Abs(f.RulesFile)
		if err != nil {
			return nil, fmt.Errorf("--egress-rules: %w", err)
		}
		out = append(out, "--egress-rules", path)
	}
	return out, nil
}

// upperFirstSentence turns a validator's fragment into the sentence the block shows: capitalized,
// ended. The validators keep their Go-style message for logs and callers that join it into one.
func upperFirstSentence(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// networkFlagExample is the spelling that works for a launch flag given with no value — the same
// flag, filled in the way its page shows it.
func networkFlagExample(command, flag string) string {
	switch flag {
	case "--allow-domain":
		return command + " --allow-domain docs.example.com"
	case "--egress-rules":
		return command + " --egress-rules .agent/egress.yaml"
	default:
		return command + " --egress filtered"
	}
}
