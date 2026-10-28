// Command lifecyclebench measures how long a Coop box really takes to become usable, and to be gone.
//
// It is a measurement tool, not telemetry: it runs the same commands a person runs, times the
// boundaries that matter, and writes the samples it took. Nothing here is installed, nothing runs in
// the background, and it never touches product code.
//
// Two boundaries, both chosen so a UI label cannot flatter them:
//
//   - start: the in-box command actually began. The box creates a file in the mounted workspace and
//     the host watches for it, so the instant is the host's own. A "Starting <agent>" line is not
//     evidence of anything.
//   - stop: nothing the run owned is left in the runtime AND the coop process has exited. A command
//     that returns while its container is still being reaped has not stopped.
//
// It removes only what its own launches made: it refuses to start while another run's coop
// containers or volumes exist, and leaves alone anything another run starts meanwhile.
//
// Usage (make runs the bench's own tests first):
//
//	make lifecycle-bench OUT=<dir> BENCH_ARGS='--samples 10'
//	go run ./tools/lifecyclebench --samples 10 --out <dir>
//	go run ./tools/lifecyclebench --cases repeat_start,normal_stop --samples 5 --out <dir>
//
// Output: <dir>/samples.json (every raw sample) and <dir>/report.md (counts, p50, p95, the machine,
// the versions, and the exact command to repeat it).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// A sample that takes longer than this is a wedged environment, not a slow one. Bounded so an
	// unattended run cannot hang the person who started it.
	caseTimeout = 300 * time.Second
	goneTimeout = 60 * time.Second // how long a finished run's own resources may take to go

	// One `docker ps` round trip is the floor on any "is it gone yet" answer, so a stop number
	// smaller than this is at the probe's resolution, not the product's.
	probeResolutionNote = "two runtime queries (~0.04s) are the floor on any gone-yet answer"
)

var (
	// errTimeout is a bound that ran out; the sampling loop files it as a failed sample.
	errTimeout = errors.New("timed out")
	// errInterrupted is a person's Ctrl-C: the sample in flight is dropped, the rest are written.
	errInterrupted = errors.New("interrupted")
)

// sample is one measurement. Its fields are in key order: samples.json sorts its keys.
type sample struct {
	Case    string   `json:"case"`
	Detail  string   `json:"detail"`
	Extra   evidence `json:"extra"`
	OK      bool     `json:"ok"`
	Seconds *float64 `json:"seconds"`
}

func measured(d time.Duration, extra ...fact) sample {
	seconds := d.Seconds()
	return sample{OK: true, Seconds: &seconds, Extra: extra}
}

func failed(detail string, extra ...fact) sample {
	return sample{Detail: detail, Extra: extra}
}

// evidence is what a case found beside its number, in the order the report lists it.
type evidence []fact

type fact struct {
	name  string
	value any
}

// MarshalJSON writes the facts as one object, its keys sorted like the rest of samples.json.
func (e evidence) MarshalJSON() ([]byte, error) {
	object := map[string]any{}
	for _, f := range e {
		object[f.name] = f.value
	}
	return marshal(object)
}

type caseResult struct {
	Boundary    string   `json:"boundary"`
	Description string   `json:"description"`
	Name        string   `json:"name"`
	Samples     []sample `json:"samples"`
	Unverified  string   `json:"unverified"`
}

// benchCase is one measurement the bench can take, and the coop flags it adds.
type benchCase struct {
	name, description, boundary string
	measure                     func(*bench, context.Context, []string) (sample, error)
	extra                       []string
}

var cases = []benchCase{
	{"repeat_start", "start with everything already warm on the host", "start", (*bench).caseStart, nil},
	{"filtered_start", "start with --egress filtered (the qualified gateway)", "start", (*bench).caseStart,
		[]string{"--egress", "filtered"}},
	{"acp_initialize", "editor start: from launching coop acp to its answer to initialize", "start",
		(*bench).caseACPInitialize, nil},
	{"acp_switch_cold", "editor provider switch to a box started for it (COOP_ACP_WARM=0)", "switch",
		(*bench).caseACPSwitchCold, nil},
	{"acp_switch_warm", "editor provider switch to the warm pool's parked box", "switch",
		(*bench).caseACPSwitchWarm, nil},
	{"normal_stop", "from the box announcing itself to nothing owned remaining", "stop", (*bench).caseStop, nil},
	{"filtered_stop", "the same, for a filtered run and its gateway", "stop", (*bench).caseStop,
		[]string{"--egress", "filtered"}},
	{"cancelled_stop", "from Ctrl-C to nothing owned remaining", "stop", (*bench).caseCancelledStop, nil},
	{"failed_start_preflight", "a launch refused before anything is created", "start",
		(*bench).caseFailedStartPreflight, nil},
	{"failed_start_after_create", "a launch that fails after the box exists, and its cleanup", "start",
		(*bench).caseFailedStartAfterCreate, nil},
}

// A cold case is deliberately absent. "Cold" would have to mean a precondition this tool does not
// control — a daemon that has not seen the image, caches discarded — and taking one sample and
// calling it cold measures the warm path under another name. Record it as not measured instead of
// measuring something else.

func main() {
	// Ctrl-C ends the sample in flight, which still stops its coop and takes back what it made; a
	// second Ctrl-C ends the bench at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	context.AfterFunc(ctx, stop)
	code := run(ctx, os.Args[1:], &bench{
		env: os.Environ(), start: (*exec.Cmd).Start, token: newToken, log: os.Stderr,
		caseTimeout: caseTimeout, goneTimeout: goneTimeout, stopSteps: stopSteps,
	}, cases, os.Stdout)
	stop()
	os.Exit(code)
}

// run is the whole command: b brings how it runs things and the flags choose the rest. The bench and
// the case table are parameters, so a test can drive the sampling loop with fake cases and a fake
// runtime.
func run(ctx context.Context, args []string, b *bench, cases []benchCase, stdout io.Writer) int {
	byName := map[string]benchCase{}
	var names []string
	for _, c := range cases {
		byName[c.name] = c
		names = append(names, c.name)
	}
	defaultCoop := "./coop"
	if found, err := exec.LookPath("coop"); err == nil {
		defaultCoop = found
	}
	cwd, _ := os.Getwd()
	flags := flag.NewFlagSet("lifecyclebench", flag.ContinueOnError)
	flags.SetOutput(b.log)
	samples := flags.Int("samples", 5, "samples per case")
	out := flags.String("out", "", "directory for samples.json and report.md (required)")
	selection := flags.String("cases", strings.Join(names, ","), "comma-separated case names")
	coop := flags.String("coop", defaultCoop, "the coop binary to measure")
	repo := flags.String("repo", cwd, "the repository to launch in")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return 0
	} else if err != nil {
		return 2
	}
	if *out == "" || flags.NArg() > 0 {
		fmt.Fprintln(b.log, "usage: go run ./tools/lifecyclebench --out <dir> [--samples N] [--cases a,b] [--coop <path>] [--repo <dir>]")
		return 2
	}
	if *samples <= 0 {
		fmt.Fprintln(b.log, "lifecyclebench: samples must be positive")
		return 2
	}
	// Absolute, always: every case runs with its working directory set to the workspace, so a
	// relative name — even one LookPath happily confirms here — resolves against the wrong directory
	// there and fails as "no such file" on the first sample.
	if b.coop = absolute(*coop); unix.Access(b.coop, unix.X_OK) != nil {
		fmt.Fprintf(b.log, "lifecyclebench: %s is not executable\n", *coop)
		return 2
	}
	b.repo = *repo
	if b.runtime = b.getenv("COOP_RUNTIME"); b.runtime == "" {
		b.runtime = "docker"
	}
	var selected, unknown []string
	for name := range strings.SplitSeq(*selection, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		} else if _, ok := byName[name]; ok {
			selected = append(selected, name)
		} else {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		fmt.Fprintf(b.log, "lifecyclebench: unknown case(s): %s\n", strings.Join(unknown, ", "))
		return 2
	}
	if len(selected) == 0 {
		fmt.Fprintln(b.log, "lifecyclebench: select at least one case")
		return 2
	}

	// Measure on a quiet runtime: another run's boxes slow every sample, and what they own is
	// theirs, which this tool must never remove.
	busy, err := b.owned()
	if err != nil {
		fmt.Fprintf(b.log, "lifecyclebench: cannot list the runtime's coop resources: %v\n", err)
		return 2
	}
	if len(busy) > 0 {
		fmt.Fprintf(b.log, "lifecyclebench: %d coop container(s) or volume(s) from other runs are present; "+
			"stop those runs (coop doctor lists the boxes) and measure on a quiet runtime\n", len(busy))
		return 2
	}

	results := []*caseResult{}
	interrupted := false
sampling:
	for _, name := range selected {
		c := byName[name]
		result := &caseResult{Name: name, Description: c.description, Boundary: c.boundary, Samples: []sample{}}
		results = append(results, result)
		for range *samples {
			s, err := c.measure(b, ctx, c.extra)
			// Samples already taken are the expensive part; losing them is the one failure mode this
			// tool must not have. Ctrl-C drops only the sample in flight, whose case has already
			// stopped its coop and taken back what it made, and the output is still written.
			if err != nil && ctx.Err() != nil {
				interrupted = true
				break sampling
			}
			if errors.Is(err, errTimeout) {
				s = failed(fmt.Sprintf("case exceeded %gs", b.caseTimeout.Seconds()))
			} else if err != nil {
				s = failed(redact(err.Error()))
			}
			s.Case = name
			result.Samples = append(result.Samples, s)
			if ctx.Err() != nil {
				interrupted = true
				break sampling
			}
		}
	}
	if interrupted {
		results[len(results)-1].Unverified = "interrupted before every case finished — the samples below are what was taken"
	}

	environment, workspace := b.collectEnvironment(), b.describeWorkspace()
	data, err := marshalIndent(struct {
		Cases       []*caseResult  `json:"cases"`
		Environment map[string]any `json:"environment"`
		Workspace   map[string]any `json:"workspace"`
	}{results, environment, workspace})
	if err == nil {
		err = os.MkdirAll(*out, 0o755)
	}
	samplesPath, reportPath := filepath.Join(*out, "samples.json"), filepath.Join(*out, "report.md")
	if err == nil {
		err = os.WriteFile(samplesPath, data, 0o644)
	}
	if err == nil {
		report := renderReport(results, environment, workspace, repeat{*samples, strings.Join(selected, ","), b.coop})
		err = os.WriteFile(reportPath, []byte(report), 0o644)
	}
	if err != nil {
		fmt.Fprintf(b.log, "lifecyclebench: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "lifecyclebench: wrote %s and %s\n", samplesPath, reportPath)
	if interrupted {
		return 1
	}
	return 0
}

// absolute is name as a resolved absolute path, found on PATH first when it is a bare command.
func absolute(name string) string {
	if found, err := exec.LookPath(name); err == nil {
		name = found
	}
	if path, err := filepath.Abs(name); err == nil {
		name = path
	}
	if resolved, err := filepath.EvalSymlinks(name); err == nil {
		name = resolved
	}
	return name
}

// result is how a short command ended and what it printed.
type result struct {
	stdout, stderr string
	code           int
}

// commandFunc runs a short command to its end, failing once timeout passes. Every runtime query and
// every metadata probe goes through one, so a test can answer them all.
type commandFunc func(timeout time.Duration, name string, args ...string) (result, error)

// Tests may inject a command runner; production calls the constructor directly
// so an aliased generic runner cannot hide a future Git subprocess from review.
func (b *bench) executeCommand(timeout time.Duration, name string, args ...string) (result, error) {
	if b.command != nil {
		return b.command(timeout, name, args...)
	}
	return runCommand(timeout, name, args...)
}

func runCommand(timeout time.Duration, name string, args ...string) (result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A process group of its own: a person's Ctrl-C ends the sample in flight, and must not also
	// kill the query or removal that takes back what the sample made.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A helper the command started can hold its output open after the command (or the timeout)
	// ended it.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		return result{}, fmt.Errorf("%s %s did not finish within %s", name, strings.Join(args, " "), timeout)
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) && !errors.Is(err, exec.ErrWaitDelay) {
		return result{}, err
	}
	return result{stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()}, nil
}

// home is this machine's home directory, which nothing retained may name.
var home, _ = os.UserHomeDir()

// tmpPath is a temp path that does not continue a word or a dotted name. RE2 has no lookbehind,
// so the character before the path is matched and put back.
var tmpPath = regexp.MustCompile(`(^|[^\p{L}\p{N}_.])/(?:private/)?(?:var|tmp)/[\p{L}\p{N}_.\-/]*`)

// redact strips this machine's identity from anything retained. Artifacts are read by other
// people: a home directory names its owner and a temp path names the run. Commit and version
// hashes are the opposite — they are the evidence — so they stay.
func redact(text string) string {
	if home != "" {
		text = strings.ReplaceAll(text, home, "~")
	}
	return tmpPath.ReplaceAllString(text, "${1}<tmp>")
}

// percentile interpolates between samples, so p95 of eight samples lands between the top two
// rather than being the slowest sample under another name. NaN when there is nothing to rank.
func percentile(values []float64, fraction float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	ordered := slices.Sorted(slices.Values(values))
	position := fraction * float64(len(ordered)-1)
	low := int(position)
	high := min(low+1, len(ordered)-1)
	return ordered[low] + (ordered[high]-ordered[low])*(position-float64(low))
}

// round is x to places decimals, rounded as its decimal form is.
func round(x float64, places int) float64 {
	rounded, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', places, 64), 64)
	return rounded
}

// describeWorkspace says what was measured IN. A near-empty directory and a real project are
// different products: a project policy can make every run filtered, and secret shadowing, image
// selection and mount assembly all scale with the tree. A number without this is not reproducible.
func (b *bench) describeWorkspace() map[string]any {
	tracked := "unknown"
	if res, err := b.executeCommand(60*time.Second, "git", "-C", b.repo, "ls-files"); err == nil && res.code == 0 {
		tracked = strconv.Itoa(strings.Count(res.stdout, "\n"))
	}
	head := "none"
	if res, err := b.executeCommand(30*time.Second, "git", "-C", b.repo, "rev-parse", "--short", "HEAD"); err == nil && res.code == 0 {
		head = strings.TrimSpace(res.stdout)
	}
	absolute, _ := filepath.Abs(b.repo)
	return map[string]any{
		// The basename survives redaction on purpose: WHICH workspace is the whole point, and
		// "<tmp>" for every one of them makes two different products look like one measurement.
		"path":               filepath.Join(redact(filepath.Dir(absolute)), filepath.Base(absolute)),
		"tracked_files":      tracked,
		"head":               head,
		"has_agent_dir":      isDir(filepath.Join(b.repo, ".agent")),
		"has_project_policy": exists(filepath.Join(b.repo, ".agent", "project.yaml")),
		"has_box_dockerfile": exists(filepath.Join(b.repo, ".agent", "Dockerfile")),
	}
}

func (b *bench) commandFirstLine(name string, args ...string) string {
	res, err := b.executeCommand(30*time.Second, name, args...)
	if err != nil {
		return "unavailable"
	}
	text := res.stdout
	if strings.TrimSpace(text) == "" {
		text = res.stderr
	}
	if line := firstOf(text); line != "" {
		return redact(line)
	}
	return "unavailable"
}

func (b *bench) collectEnvironment() map[string]any {

	// The binary's OWN stamp, not the checkout's HEAD: they are routinely different, and a report
	// that names a commit the measured binary was not built from is worse than one that admits it.
	version := b.commandFirstLine(b.coop, "version")
	memory := "unknown"
	if runtime.GOOS == "darwin" {
		if res, err := b.executeCommand(10*time.Second, "sysctl", "-n", "hw.memsize"); err == nil {
			if size, err := strconv.ParseInt(strings.TrimSpace(res.stdout), 10, 64); err == nil {
				memory = fmt.Sprintf("%d GiB", size>>30)
			}
		}
	} else if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		if fields := strings.Fields(firstOf(string(data))); len(fields) > 1 {
			if kib, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				memory = fmt.Sprintf("%d GiB", kib>>20)
			}
		}
	}
	// The daemon, not the host: a Linux container on macOS runs inside the runtime's VM, which has
	// its own CPU and memory budget and its own clock. Best-effort, so a missing or slow runtime
	// still leaves the samples their metadata.
	daemon := "unknown"
	if res, err := b.executeCommand(60*time.Second, b.runtime, "info", "--format",
		"{{.OperatingSystem}} / {{.ServerVersion}} / {{.NCPU}} cpus / {{.MemTotal}} bytes"); err == nil &&
		res.code == 0 && strings.TrimSpace(res.stdout) != "" {
		daemon = redact(firstOf(res.stdout))
	}
	provenance := "built from a committed tree"
	if strings.Contains(version, "dirty") {
		provenance = "built from a tree with uncommitted changes — cite the commit it became"
	}
	hostOS := "unknown"
	var uname unix.Utsname
	if unix.Uname(&uname) == nil {
		hostOS = unix.ByteSliceToString(uname.Sysname[:]) + " " + unix.ByteSliceToString(uname.Release[:]) + " " +
			unix.ByteSliceToString(uname.Machine[:])
	}
	return map[string]any{
		"coop_version":      version,
		"binary_provenance": provenance,
		"runtime":           b.runtime,
		"runtime_version":   b.commandFirstLine(b.runtime, "--version"),
		"runtime_daemon":    daemon,
		"host_os":           hostOS,
		"host_cpus":         runtime.NumCPU(),
		"host_memory":       memory,
		"measured_at_utc":   time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// firstOf is the first line of text once its surrounding blank space is trimmed.
func firstOf(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(line)
}

// repeat is what the report's command line needs to take the same measurement again.
type repeat struct {
	samples int
	cases   string
	coop    string
}

func renderReport(results []*caseResult, environment, workspace map[string]any, again repeat) string {
	lines := []string{"# Agent lifecycle baseline", "",
		"Start is when the host saw the box announce itself; stop is when nothing the run owned",
		"is left in the runtime. Both instants are the host's own clock, never a UI line and",
		"never the container's clock.", "", "## Machine", ""}
	for _, key := range []string{"coop_version", "binary_provenance", "runtime", "runtime_version", "runtime_daemon",
		"host_os", "host_cpus", "host_memory", "measured_at_utc"} {
		lines = append(lines, fmt.Sprintf("- **%s:** %s", key, valueOr(environment, key)))
	}
	lines = append(lines, "", "## Workspace", "")
	for _, key := range []string{"path", "head", "tracked_files", "has_agent_dir", "has_project_policy",
		"has_box_dockerfile"} {
		lines = append(lines, fmt.Sprintf("- **%s:** %s", key, valueOr(workspace, key)))
	}
	lines = append(lines, "", "## Results", "",
		"| case | boundary | ok | n | p50 (s) | p95 (s) | min (s) | max (s) |",
		"|---|---|---|---|---|---|---|---|")
	for _, c := range results {
		var ok []float64
		for _, s := range c.Samples {
			if s.OK && s.Seconds != nil {
				ok = append(ok, *s.Seconds)
			}
		}
		if len(ok) > 0 {
			lines = append(lines, fmt.Sprintf("| %s | %s | %d/%d | %d | %.3f | %.3f | %.3f | %.3f |",
				c.Name, c.Boundary, len(ok), len(c.Samples), len(c.Samples), percentile(ok, 0.5),
				percentile(ok, 0.95), slices.Min(ok), slices.Max(ok)))
		} else {
			// A case with nothing to average shows dashes, not a NaN dressed up as a percentile.
			lines = append(lines, fmt.Sprintf("| %s | %s | 0/%d | %d | — | — | — | — |",
				c.Name, c.Boundary, len(c.Samples), len(c.Samples)))
		}
	}
	lines = append(lines, "", "## Notes", "")
	for _, c := range results {
		lines = append(lines, fmt.Sprintf("- **%s** — %s", c.Name, c.Description))
		if c.Unverified != "" {
			lines = append(lines, "  - unverified: "+c.Unverified)
		}
		for _, s := range c.Samples {
			var facts []string
			for _, f := range s.Extra {
				facts = append(facts, f.name+"="+format(f.value))
			}
			said := strings.Join(facts, ", ")
			if !s.OK {
				line := "  - failed sample: " + s.Detail
				if said != "" {
					line += " (" + said + ")"
				}
				lines = append(lines, line)
			} else if said != "" {
				lines = append(lines, "  - "+said)
			}
		}
	}
	lines = append(lines, "", "## Repeat it", "", "```",
		fmt.Sprintf("go run ./tools/lifecyclebench --samples %d --cases %s \\", again.samples, again.cases),
		fmt.Sprintf("  --coop %s --repo %s --out <dir>", redact(again.coop), valueOr(workspace, "path")),
		"```",
		"",
		"The workspace is part of the measurement, not a detail: a project whose policy makes",
		"every run filtered pays a different start than a bare directory, and this command",
		"names the one these numbers came from.", "")
	return strings.Join(lines, "\n")
}

func valueOr(values map[string]any, key string) string {
	if value, ok := values[key]; ok {
		return format(value)
	}
	return "unknown"
}

// format writes a value the way the report always has: True/False, and a float with a decimal
// point even when it is whole. A structured value is written as JSON.
func format(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(v)
	case float64:
		text := strconv.FormatFloat(v, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	default:
		data, err := marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(data)
	}
}

// marshal is json.Marshal without HTML escaping: "<tmp>" stays readable in samples.json.
func marshal(value any) ([]byte, error) {
	data, err := encode(value, "")
	return bytes.TrimSuffix(data, []byte("\n")), err
}

func marshalIndent(value any) ([]byte, error) {
	return encode(value, "  ")
}

func encode(value any, indent string) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", indent)
	err := encoder.Encode(value)
	return out.Bytes(), err
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
