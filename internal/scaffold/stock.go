package scaffold

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/project"
)

// Stock reports whether rel, a repository-relative slash-separated path, holds exactly what this
// Coop's init writes there: a template's bytes, a hook or commit gate for some choice of
// languages, the project settings for this repository's members and some choice of services, the
// Compose file for a choice of services, the Dockerfile for its .tool-versions, or the link init
// makes. coop check-secrets leaves these out of the files it asks a person to review: they are
// Coop's own, and sixteen of them buried a fresh project's secret result. A file that differs by
// one byte, or a link to anywhere else, is not stock.
func Stock(repo, rel string) bool {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return false
	}
	defer root.Close()
	name := filepath.FromSlash(rel)
	info, err := root.Lstat(name)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(name)
		return err == nil && target != "" && target == stockLink(repo, rel)
	}
	candidates := stockContents(repo, rel)
	if len(candidates) == 0 {
		return false
	}
	file, _, err := openRegular(root, name, rel)
	if err != nil {
		return false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxReadBytes+1))
	return err == nil && slices.Contains(candidates, string(data))
}

// stockLink is the target of the link init makes at rel, or "" when it makes none there: an
// instruction file pointing at AGENTS.md, or an agent's skills folder pointing at the shared one.
func stockLink(repo, rel string) string {
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		dir := ag.Scaffold().Project.Dir
		if dir == "" {
			continue
		}
		if instruction := ag.InstructionFile(); instruction != "" && instruction != "AGENTS.md" && rel == instruction {
			return "AGENTS.md"
		}
		if ag.SkillsCapable() && rel == dir+"/skills" {
			target, err := filepath.Rel(filepath.Join(repo, dir), skillsSource(repo))
			if err != nil {
				return ""
			}
			return target
		}
	}
	return ""
}

// stockContents is every content init can write at rel. Where a person's answers shape the file,
// it holds the file for each possible answer; where the repository does, the file for the
// repository as it is now.
func stockContents(repo, rel string) []string {
	var out []string
	template := func(src string) {
		if data, err := templates.ReadFile(src); err == nil {
			out = append(out, string(data))
		}
	}
	switch rel {
	case "AGENTS.md":
		template("templates/AGENTS.md")
	case ".agent/tasks/README.md":
		template("templates/agent/tasks/README.md")
	case ".agent/loop.yaml":
		template("templates/agent/loop.yaml")
	case ".githooks/pre-commit":
		for _, langs := range gateChoices() {
			out = append(out, preCommitHook(langs))
		}
	case ".githooks/prepare-commit-msg":
		out = append(out, prepareCommitMsgChainHook)
	case project.File:
		subs, err := DetectSubprojects(repo)
		if err != nil {
			return nil
		}
		entries, err := subprojectEntries(subs)
		if err != nil {
			return nil
		}
		for _, services := range serviceChoices() {
			out = append(out, projectYAML(entries, services...))
		}
	case project.DefaultCompose:
		for _, services := range serviceChoices() {
			if compose := composeFor(services); compose != "" {
				out = append(out, compose)
			}
		}
	case project.DefaultDockerfile:
		if dockerfile, _, err := initDockerfile(repo, "asdf"); err == nil {
			out = append(out, dockerfile)
		}
	}
	if skill, ok := strings.CutPrefix(rel, ".agent/skills/"); ok {
		template("templates/skills/" + skill)
	}
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		spec := ag.Scaffold()
		for _, layout := range []agents.ScaffoldLayout{spec.Project, spec.Fallback} {
			for _, f := range layout.Files {
				if f.Path == rel {
					template(f.Template)
				}
			}
			if spec.CommitGate != nil && layout.CommitGatePath == rel {
				for _, langs := range gateChoices() {
					out = append(out, spec.CommitGate.Prefix+gateBody(langs, spec.CommitGate.FailureCode)+spec.CommitGate.Suffix)
				}
			}
		}
	}
	return out
}

// gateChoices is every language list a gate can be generated for. Detection lists GateLangs in
// order, but the prompt keeps the order a person typed, so this is every ordered selection of
// distinct languages, the empty one (the neutral gate) included.
func gateChoices() [][]string {
	var out [][]string
	var pick func(chosen []string)
	pick = func(chosen []string) {
		out = append(out, chosen)
		for _, lang := range GateLangs {
			if !slices.Contains(chosen, lang) {
				pick(append(slices.Clip(chosen), lang))
			}
		}
	}
	pick(nil)
	return out
}

// serviceChoices is every subset of ComposeServices. The generated files list services in catalog
// order whatever order they were chosen in, so subsets are enough.
func serviceChoices() [][]string {
	out := [][]string{nil}
	for _, service := range ComposeServices {
		for _, chosen := range out {
			out = append(out, append(slices.Clip(chosen), service))
		}
	}
	return out
}
