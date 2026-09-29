package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The change-size metrics answer a review question, never a scoring one: did two equal-quality
// solutions involve substantially different amounts of code or rework? Size is reported BESIDE
// correctness, never as an automatic over-engineering penalty — a smaller incorrect solution is not
// better, and tests, docs and clear code legitimately add lines.
//
// Measure per-language code/comment/blank totals with `cloc --json`, then derive net growth from
// the before/after totals. The tool is pinned and ambient config is refused for reproducible counts.
//
// A missing cloc, an unparseable output or an unknown language is an explicit measurement error or
// unknown-coverage note, never a silent zero.

// clocFlags are the explicit cloc options: machine-readable, quiet, and counting every path
// (--skip-uniqueness, so two identical files are two files, not one — the spec counts per path).
// Ambient config (~/.config/cloc/options.txt) is neutralized by running under a hermetic HOME in
// runCloc; the tool itself is pinned at the preparation step, not here.
var clocFlags = []string{"--json", "--quiet", "--skip-uniqueness"}

// LangCount is one language's line tally in a snapshot.
type LangCount struct {
	Files   int `json:"files"`
	Blank   int `json:"blank"`
	Comment int `json:"comment"`
	Code    int `json:"code"`
}

// SizeMetrics is a snapshot's per-language counts plus the cloc version that produced them, so a
// comparison can refuse to rank two measurements taken with different tooling.
type SizeMetrics struct {
	ClocVersion string               `json:"cloc_version"`
	Languages   map[string]LangCount `json:"languages"`
	// Skipped names paths NOT measured because they are not regular files (a symlink or a device):
	// a candidate creates symlinks freely, and following them would both escape the workspace and
	// let the candidate steer the count, so they are recorded as unmeasured, never as zero.
	Skipped []string `json:"skipped,omitempty"`
	// Ignored is cloc's own list of files it could not classify (unknown language, too large): an
	// explicit measurement gap, not a silent zero.
	Ignored []string `json:"ignored,omitempty"`
}

// TotalCode sums code across languages.
func (m SizeMetrics) TotalCode() int { return m.sum(func(c LangCount) int { return c.Code }) }

// TotalComment and TotalBlank likewise.
func (m SizeMetrics) TotalComment() int { return m.sum(func(c LangCount) int { return c.Comment }) }
func (m SizeMetrics) TotalBlank() int   { return m.sum(func(c LangCount) int { return c.Blank }) }

func (m SizeMetrics) sum(pick func(LangCount) int) int {
	total := 0
	for _, c := range m.Languages {
		total += pick(c)
	}
	return total
}

// NetCodeGrowth is after-minus-before code, derived from the two snapshots' totals — the honest net,
// which cloc's "modified" count cannot give.
func NetCodeGrowth(before, after SizeMetrics) int { return after.TotalCode() - before.TotalCode() }

// parseClocJSON parses `cloc --json` output into per-language counts, dropping cloc's own "header"
// and "SUM" pseudo-entries (SUM is re-derivable, and mixing it into the language map would double
// every total). An empty tree (cloc emits just a header, or nothing) yields an empty, non-nil map.
func parseClocJSON(data []byte) (SizeMetrics, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return SizeMetrics{Languages: map[string]LangCount{}}, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return SizeMetrics{}, fmt.Errorf("parse cloc json: %w", err)
	}
	out := SizeMetrics{Languages: map[string]LangCount{}}
	if h, ok := raw["header"]; ok {
		var header struct {
			ClocVersion string `json:"cloc_version"`
		}
		_ = json.Unmarshal(h, &header)
		out.ClocVersion = header.ClocVersion
	}
	for lang, blob := range raw {
		if lang == "header" || lang == "SUM" {
			continue
		}
		var c clocLang
		if err := json.Unmarshal(blob, &c); err != nil {
			return SizeMetrics{}, fmt.Errorf("parse cloc language %q: %w", lang, err)
		}
		out.Languages[lang] = c.count()
	}
	return out, nil
}

// clocLang mirrors cloc's per-language object (nFiles, not files).
type clocLang struct {
	NFiles  int `json:"nFiles"`
	Blank   int `json:"blank"`
	Comment int `json:"comment"`
	Code    int `json:"code"`
}

func (c clocLang) count() LangCount {
	return LangCount{Files: c.NFiles, Blank: c.Blank, Comment: c.Comment, Code: c.Code}
}

// clocTimeout bounds a cloc run over a trial's workspace.
const clocTimeout = 2 * time.Minute

// MeasureSize measures dir's per-language line counts. It never runs cloc directly on the workspace:
// it first projects the REGULAR FILES ONLY into a private temp tree (skipping every symlink and
// special file, recording them), then runs cloc on that. So a candidate's symlink can neither make
// cloc read outside the workspace nor steer the count, and a missing/empty tree is an explicit
// result, not a silent zero. Best-effort at the call site: a run error is a measurement gap.
func MeasureSize(ctx context.Context, dir string, ignore ...string) (SizeMetrics, error) {
	proj, skipped, err := projectRegularFiles(ctx, dir, ignore...)
	if err != nil {
		return SizeMetrics{}, err
	}
	defer os.RemoveAll(proj)
	ignored, err := os.CreateTemp("", "coop-eval-cloc-ignored-")
	if err != nil {
		return SizeMetrics{}, err
	}
	ignored.Close()
	defer os.Remove(ignored.Name())
	out, err := runCloc(ctx, append(append([]string{}, clocFlags...), "--ignored="+ignored.Name(), proj)...)
	if err != nil {
		return SizeMetrics{}, err
	}
	m, err := parseClocJSON(out)
	if err != nil {
		return SizeMetrics{}, err
	}
	m.Skipped = skipped
	m.Ignored = readIgnored(ignored.Name())
	return m, nil
}

// projectRegularFiles copies every regular file under src into a fresh temp directory, preserving
// relative paths, and returns the temp dir plus the relative paths it SKIPPED (symlinks, devices,
// sockets — anything not a regular file or directory). A missing or non-directory src is an error,
// so an unmeasurable input is never mistaken for an empty one.
func projectRegularFiles(ctx context.Context, src string, ignore ...string) (string, []string, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	info, err := os.Stat(src)
	if err != nil {
		return "", nil, fmt.Errorf("measure %q: %w", src, err)
	}
	if !info.IsDir() {
		return "", nil, fmt.Errorf("measure %q: not a directory", src)
	}
	dst, err := os.MkdirTemp("", "coop-eval-cloc-proj-")
	if err != nil {
		return "", nil, err
	}
	var skipped []string
	err = filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if isIgnored(rel, ignore) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			skipped = append(skipped, rel)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		case d.IsDir():
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		case fi.Mode().IsRegular():
			return copyFile(ctx, path, filepath.Join(dst, rel), fi.Mode().Perm())
		default:
			skipped = append(skipped, rel) // device, socket, fifo — not measurable
			return nil
		}
	})
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		os.RemoveAll(dst)
		return "", nil, err
	}
	sort.Strings(skipped)
	return dst, skipped, nil
}

// readIgnored parses cloc's --ignored file (with --json it is a JSON array of {file, reason}) into
// the relative file names cloc could not measure. Best-effort: an unreadable list is simply empty.
func readIgnored(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	var entries []struct {
		File string `json:"file"`
	}
	if json.Unmarshal(data, &entries) != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, filepath.Base(e.File))
	}
	sort.Strings(out)
	return out
}

func runCloc(ctx context.Context, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, clocTimeout)
	defer cancel()
	home, err := os.MkdirTemp("", "coop-eval-cloc-home-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	cmd := exec.CommandContext(ctx, "cloc", args...)
	// A hermetic environment: an empty HOME/XDG so no ambient ~/.config/cloc/options.txt steers the
	// counts; PATH is the only host thing cloc needs (it may shell out to a language filter).
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("run cloc: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("run cloc: %w", err)
	}
	return out, nil
}
