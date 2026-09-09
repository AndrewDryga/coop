package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
)

type networkFlags struct {
	Mode      *egress.Mode
	Domains   []string
	RulesFile string
}

// Coop flags end at its first separator. Mode presence is retained: the shared
// admission boundary, not the parser, resolves remembered posture and conflicts.
func extractNetworkFlags(args []string) (networkFlags, []string, error) {
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
				return networkFlags{}, nil, err
			}
			switch name {
			case "--egress":
				if flags.Mode != nil {
					return networkFlags{}, nil, errors.New("--egress may be supplied only once")
				}
				mode, err := egress.ParseMode(value)
				if err != nil {
					return networkFlags{}, nil, err
				}
				flags.Mode = &mode
			case "--allow-domain":
				if len(flags.Domains) >= egress.MaxRules {
					return networkFlags{}, nil, fmt.Errorf("--allow-domain accepts at most %d entries", egress.MaxRules)
				}
				domain, err := egress.NormalizeDomain(value, false)
				if err != nil {
					return networkFlags{}, nil, fmt.Errorf("--allow-domain: %w", err)
				}
				flags.Domains = append(flags.Domains, domain)
			case "--egress-rules":
				if flags.RulesFile != "" {
					return networkFlags{}, nil, errors.New("--egress-rules may be supplied only once")
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
