// Command fsprobe qualifies fixed filesystem fixtures. It never changes launch policy.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	cruntime "github.com/AndrewDryga/coop/internal/runtime"
)

//go:embed assets/*
var assets embed.FS

type observation struct {
	Layout, Stage string
	Evidence      json.RawMessage
}

type report struct {
	Version                        int
	Time                           time.Time
	Mode                           string
	Share                          string
	Fixture                        string
	ShareIdentity, FixtureIdentity fileIdentity
	Endpoint                       string
	Daemon                         cruntime.DockerInfo
	Image                          string
	Mounts                         map[string][]cruntime.DockerMount
	Containers                     []cruntime.DockerRef
	Observations                   []observation
	Complete                       bool
	Qualified                      bool
	Limitations                    []string
	Error                          string `json:",omitempty"`
}

type fileIdentity struct{ Device, Inode uint64 }

func identity(info os.FileInfo) fileIdentity {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}
	}
	return fileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
}

type fixture struct {
	parent, root *os.Root
	name, path   string
	identity     os.FileInfo
	written      map[string]bool
	binds        map[string]os.FileInfo
}

func newFixture(share string) (*fixture, error) {
	parent, err := os.OpenRoot(share)
	if err != nil {
		return nil, err
	}
	f := &fixture{parent: parent, name: ".coop-fsprobe-" + rand.Text(), written: map[string]bool{}, binds: map[string]os.FileInfo{}}
	if err = parent.Mkdir(f.name, 0700); err != nil {
		parent.Close()
		return nil, err
	}
	f.path = filepath.Join(share, f.name)
	f.root, f.identity, err = openFixture(parent, f.name)
	if err != nil {
		parent.Close()
		return nil, err
	}
	return f, nil
}

func openFixture(parent *os.Root, name string) (*os.Root, os.FileInfo, error) {
	before, err := parent.Lstat(name)
	if err != nil || !before.IsDir() {
		return nil, nil, errors.Join(errors.New("fixture is not a real directory"), err)
	}
	// OpenRoot alone follows in-root symlinks. Compare its pinned object with a
	// no-follow observation before performing even the first fixture write.
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	after, err := root.Stat(".")
	current, currentErr := parent.Lstat(name)
	if err != nil || currentErr != nil || !current.IsDir() || !os.SameFile(before, after) || !os.SameFile(before, current) {
		root.Close()
		return nil, nil, errors.Join(errors.New("fixture changed during acquisition"), err, currentErr)
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil || len(entries) != 0 {
		root.Close()
		return nil, nil, errors.Join(errors.New("fixture was not empty"), err)
	}
	return root, after, nil
}

func (f *fixture) verify() error {
	now, err := f.parent.Lstat(f.name)
	if err != nil || !os.SameFile(f.identity, now) || !now.IsDir() {
		return errors.Join(errors.New("fixture pathname identity changed"), err)
	}
	return nil
}

func (f *fixture) verifyBind(base string) error {
	if err := f.verify(); err != nil {
		return err
	}
	path := ""
	for _, part := range strings.Split(base, "/") {
		path = filepath.Join(path, part)
		now, err := f.root.Lstat(path)
		before := f.binds[path]
		if err != nil || before == nil || !now.IsDir() || !os.SameFile(before, now) {
			return errors.Join(errors.New("fixture bind source changed: "+path), err)
		}
	}
	return nil
}

func (f *fixture) close() error {
	defer f.parent.Close()
	defer f.root.Close()
	// Clean only through the pinned root. Never recursively follow a replacement
	// at its former pathname, even when evidence gathering failed.
	if err := f.verify(); err != nil {
		return err
	}
	var paths []string
	err := fs.WalkDir(f.root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		if !fixturePath(path) {
			return fmt.Errorf("unrecognized fixture entry preserved: %s", path)
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(paths) - 1; i >= 0; i-- {
		if err := f.root.Remove(paths[i]); err != nil {
			return err
		}
	}
	if err := f.verify(); err != nil {
		return err
	}
	return f.parent.Remove(f.name)
}

func fixturePath(path string) bool {
	if path == "empty" || path == "empty-dir" || path == "policy" || path == "workspace" {
		return true
	}
	var ok bool
	path, ok = strings.CutPrefix(path, "workspace/")
	if !ok {
		return false
	}
	for _, parent := range []string{"nested", "old-nested"} {
		if path == parent {
			return true
		}
		if tail, found := strings.CutPrefix(path, parent+"/"); found {
			path = tail
			break
		}
	}
	if path == "private" || path == "private/key" {
		return true
	}
	if strings.EqualFold(path, ".coopignore") {
		return true
	}
	for _, prefix := range []string{"policy-link-", "policy-hard-"} {
		if strings.HasPrefix(path, prefix) && len(path) == len(prefix)+1 && path[len(prefix)] >= '0' && path[len(prefix)] <= '5' {
			return true
		}
	}
	for _, name := range []string{".env", ".ENV", ".COOPIGNORE", "Café.pem", "key.pem", ".coopignore", "identity", "public.txt", "env-link", "env-hard", "policy-link", "policy-hard", "replacement-hard", "editor-save", "new-public", "renamed-public", "policy-save", "moved-policy", "new-policy-link"} {
		if path == name {
			return true
		}
	}
	return false
}

func (f *fixture) write(name, body string) error {
	if err := f.verify(); err != nil {
		return err
	}
	if info, err := f.root.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return errors.New("fixture file replaced by non-regular entry")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | syscall.O_NOFOLLOW
	if f.written[name] {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	file, err := f.root.OpenFile(name, flags, 0666)
	if err != nil {
		return err
	}
	f.written[name] = true
	_, writeErr := file.WriteString(body)
	return errors.Join(writeErr, file.Close())
}

func (f *fixture) seed() error {
	for _, dir := range []string{"workspace", "workspace/nested", "workspace/private", "workspace/nested/private", "empty-dir"} {
		if err := f.root.Mkdir(dir, 0777); err != nil {
			return err
		}
		if err := f.root.Chmod(dir, 0777); err != nil {
			return err
		}
		info, err := f.root.Lstat(dir)
		if err != nil {
			return err
		}
		f.binds[dir] = info
	}
	for _, base := range []string{"workspace", "workspace/nested"} {
		for name, body := range map[string]string{
			".env": "INERT_SECRET", "Café.pem": "INERT_SECRET", "key.pem": "INERT_SECRET",
			"private/key": "INERT_SECRET", ".coopignore": "fixture policy\n", "public.txt": "PUBLIC",
			"identity": f.name,
		} {
			if err := f.write(base+"/"+name, body); err != nil {
				return err
			}
			if err := f.root.Chmod(base+"/"+name, 0666); err != nil {
				return err
			}
		}
		for alias, target := range map[string]string{"env-link": ".env", "policy-link": ".coopignore"} {
			if err := f.root.Symlink(target, base+"/"+alias); err != nil {
				return err
			}
		}
		for alias, target := range map[string]string{"env-hard": ".env", "policy-hard": ".coopignore"} {
			if err := f.root.Link(base+"/"+target, base+"/"+alias); err != nil {
				return err
			}
		}
	}
	if err := errors.Join(f.write("empty", ""), f.write("policy", "fixture policy\n")); err != nil {
		return err
	}
	return f.root.Chmod(".", 0755)
}

type runner struct {
	docker               *cruntime.Docker
	image, source, token string
	refs                 []cruntime.DockerRef
	programIDs           []uint32
	report               *report
}

func mount(source, target string, readonly bool) (string, error) {
	if strings.ContainsAny(source+target, ",\x00\r\n") {
		return "", errors.New("fixture mount path cannot contain commas or control characters")
	}
	value := "type=bind,src=" + source + ",dst=" + target
	if readonly {
		value += ",readonly"
	}
	return value, nil
}

func (r *runner) create(ctx context.Context, name string, options, command []string) (cruntime.DockerRef, error) {
	ref := cruntime.DockerRef{Name: "coop-fsprobe-" + r.token + "-" + name, Labels: map[string]string{"coop.fsprobe": r.token}}
	// Keep the intention even when create returns an unknown outcome.
	r.refs = append(r.refs, ref)
	r.report.Containers = append(r.report.Containers, ref)
	id, err := r.docker.CreateContainer(ctx, cruntime.DockerCreate{Ref: ref, Image: r.image, Options: options, Command: command})
	if id != "" {
		ref.ID = id
		r.refs[len(r.refs)-1] = ref
		r.report.Containers[len(r.report.Containers)-1] = ref
	}
	return ref, err
}

func (r *runner) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var result error
	for i := len(r.refs) - 1; i >= 0; i-- {
		ref := r.refs[i]
		if ref.ID == "" {
			value, present, err := r.docker.InspectContainer(ctx, ref)
			if err != nil {
				result = errors.Join(result, err)
				continue
			}
			if !present {
				continue
			}
			ref.ID = value.ID
		}
		result = errors.Join(result, r.docker.RemoveContainer(ctx, ref))
	}
	return result
}

func (r *runner) workload(ctx context.Context, f *fixture, name, base, target string, masked bool) (cruntime.DockerRef, error) {
	if err := f.verifyBind(base); err != nil {
		return cruntime.DockerRef{}, err
	}
	options := []string{"--network", "none", "--read-only", "--no-healthcheck", "--user", "1000:1000", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "32", "--memory", "256m", "--entrypoint", "/usr/bin/python3"}
	binds := [][3]string{{filepath.Join(f.path, base), target, ""}, {r.source, "/probe", "ro"}}
	if masked {
		for _, name := range []string{".env", "Café.pem", "key.pem"} {
			binds = append(binds, [3]string{filepath.Join(f.path, "empty"), target + "/" + name, "ro"})
		}
		binds = append(binds, [3]string{filepath.Join(f.path, "empty-dir"), target + "/private", "ro"}, [3]string{filepath.Join(f.path, "policy"), target + "/.coopignore", "ro"})
	}
	for _, bind := range binds {
		value, err := mount(bind[0], bind[1], bind[2] == "ro")
		if err != nil {
			return cruntime.DockerRef{}, err
		}
		options = append(options, "--mount", value)
	}
	ref, err := r.create(ctx, name, options, []string{"-c", "import time; time.sleep(600)"})
	if err != nil {
		return ref, err
	}
	if err = r.docker.StartContainer(ctx, ref); err != nil {
		return ref, err
	}
	value, present, err := r.docker.InspectContainer(ctx, ref)
	if err != nil || !present || value.Image != r.image || value.User != "1000:1000" || value.Privileged || value.NetworkMode != "none" || !value.ReadonlyRootfs {
		return ref, errors.Join(errors.New("workload identity or posture changed"), err)
	}
	r.report.Mounts[name] = value.Mounts
	return ref, f.verifyBind(base)
}

func (r *runner) probe(ctx context.Context, ref cruntime.DockerRef, target, stage string, args ...string) (json.RawMessage, error) {
	step, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := append([]string{"/usr/bin/python3", "/probe/probe.py", target}, args...)
	data, err := r.docker.ExecRead(step, ref, 128<<10, command...)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, errors.New("invalid or incomplete probe evidence")
	}
	r.report.Observations = append(r.report.Observations, observation{Layout: ref.Name, Stage: stage, Evidence: json.RawMessage(data)})
	return data, nil
}

func cgroupIdentity(data []byte, marker string) (uint64, error) {
	var value struct {
		Cgroup  uint64
		Fixture struct{ Result, Body string }
	}
	if json.Unmarshal(data, &value) != nil || value.Cgroup == 0 || value.Fixture.Result != "read" || value.Fixture.Body != marker {
		return 0, errors.New("fixture or cgroup identity unavailable")
	}
	return value.Cgroup, nil
}

func (r *runner) attach(ctx context.Context, f *fixture, groups [2]uint64) error {
	if err := f.verifyBind("workspace/nested"); err != nil {
		return err
	}
	options := []string{"--network", "none", "--read-only", "--no-healthcheck", "--user", "0", "--cap-drop", "ALL", "--cap-add", "SYS_ADMIN", "--security-opt", "seccomp=unconfined", "--pids-limit", "32", "--memory", "512m", "--tmpfs", "/tmp:exec", "--entrypoint", "/bin/sh"}
	for _, bind := range [][2]string{{r.source, "/probe"}, {filepath.Join(f.path, "workspace"), "/fixture"}, {"/sys/fs/cgroup", "/host-cgroup"}} {
		value, err := mount(bind[0], bind[1], true)
		if err != nil {
			return err
		}
		options = append(options, "--mount", value)
	}
	ref, err := r.create(ctx, "loader", options, []string{"-c", "gcc -O2 -Wall -Wextra -Werror /probe/loader.c -o /tmp/loader && exec timeout 30 /tmp/loader \"$1\" \"$2\"", "loader", strconv.FormatUint(groups[0], 10), strconv.FormatUint(groups[1], 10)})
	if err != nil {
		return err
	}
	var stdout, stderr boundedBuffer
	code, err := r.docker.StartAttached(ctx, ref, nil, &stdout, &stderr, nil)
	r.report.Observations = append(r.report.Observations, observation{Layout: "loader", Stage: "attach", Evidence: raw(map[string]any{"exit": code, "stdout": stdout.String(), "stderr": stderr.String()})})
	if err != nil || code != 0 || stdout.overflow || stderr.overflow {
		return errors.Join(errors.New("BPF candidate unavailable or attachment incomplete: "+stderr.String()), err)
	}
	var proof struct {
		Attached   int
		ProgramIDs []uint32 `json:"program_ids"`
	}
	if json.Unmarshal([]byte(stdout.String()), &proof) != nil || proof.Attached != 12 || len(proof.ProgramIDs) != 6 {
		return errors.New("missing attachment evidence")
	}
	r.programIDs = proof.ProgramIDs
	// Prove actual access only after the privileged loader container is gone.
	return r.docker.RemoveContainer(ctx, ref)
}

func (r *runner) programsGone() error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	value, err := mount(r.source, "/probe", true)
	if err != nil {
		return err
	}
	options := []string{"--network", "none", "--read-only", "--no-healthcheck", "--user", "0", "--cap-drop", "ALL", "--cap-add", "SYS_ADMIN", "--security-opt", "seccomp=unconfined", "--pids-limit", "32", "--memory", "512m", "--tmpfs", "/tmp:exec", "--entrypoint", "/bin/sh", "--mount", value}
	command := []string{"-c", "gcc -O2 -Wall -Wextra -Werror /probe/loader.c -o /tmp/loader && exec timeout 20 /tmp/loader --check-gone \"$@\"", "cleanup"}
	for _, id := range r.programIDs {
		command = append(command, strconv.FormatUint(uint64(id), 10))
	}
	ref, err := r.create(ctx, "cleanup", options, command)
	if err != nil {
		return errors.Join(err, r.cleanup())
	}
	var stdout, stderr boundedBuffer
	code, runErr := r.docker.StartAttached(ctx, ref, nil, &stdout, &stderr, nil)
	r.report.Observations = append(r.report.Observations, observation{Layout: "loader", Stage: "kernel-cleanup", Evidence: raw(map[string]any{"exit": code, "stdout": stdout.String(), "stderr": stderr.String()})})
	var proof struct {
		Gone int `json:"programs_gone"`
	}
	if runErr == nil && (code != 0 || stdout.overflow || stderr.overflow || json.Unmarshal([]byte(stdout.String()), &proof) != nil || proof.Gone != 6) {
		runErr = errors.New("kernel program cleanup unconfirmed: " + stderr.String())
	}
	return errors.Join(runErr, r.cleanup())
}

type boundedBuffer struct {
	value    strings.Builder
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.value.Len()+n > 128<<10 {
		b.overflow = true
		p = p[:max(0, (128<<10)-b.value.Len())]
	}
	_, _ = b.value.Write(p)
	return n, nil
}
func (b *boundedBuffer) String() string { return b.value.String() }
func raw(value any) json.RawMessage     { data, _ := json.Marshal(value); return data }

func run(ctx context.Context, share, image string, bpf bool, result *report) (err error) {
	share, err = filepath.Abs(share)
	if err != nil {
		return err
	}
	share, err = filepath.EvalSymlinks(share)
	if err != nil {
		return err
	}
	result.Share = share
	f, err := newFixture(share)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.close()) }()
	result.Fixture = f.name
	parentInfo, err := f.parent.Stat(".")
	if err != nil {
		return err
	}
	result.ShareIdentity, result.FixtureIdentity = identity(parentInfo), identity(f.identity)
	if err = f.seed(); err != nil {
		return err
	}
	source, err := os.MkdirTemp("", "coop-fsprobe-code-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(source)) }()
	if err = os.Chmod(source, 0755); err != nil {
		return err
	}
	for _, name := range []string{"probe.py", "loader.c"} {
		data, readErr := assets.ReadFile("assets/" + name)
		if readErr != nil {
			return readErr
		}
		if err = os.WriteFile(filepath.Join(source, name), data, 0644); err != nil {
			return err
		}
	}
	docker, err := cruntime.BindDockerForSetup(ctx, cruntime.Runtime{Name: "docker"}, "", "")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, docker.Close()) }()
	result.Daemon, result.Endpoint = docker.Info(), docker.Endpoint()
	result.Image, _, err = docker.Image(ctx, image)
	if err != nil {
		return err
	}
	r := &runner{docker: docker, image: result.Image, source: source, token: strings.ToLower(rand.Text()[:12]), report: result}
	defer func() {
		cleanupErr := r.cleanup()
		err = errors.Join(err, cleanupErr)
		if cleanupErr == nil && len(r.programIDs) > 0 {
			err = errors.Join(err, r.programsGone())
		}
	}()
	return r.experiments(ctx, f, bpf)
}

func main() {
	share := flag.String("share", "", "existing host directory whose Docker share to test")
	image := flag.String("image", "", "cached trusted Coop clients image (never pulled)")
	out := flag.String("out", "", "JSON evidence path outside the fixture")
	bpf := flag.Bool("bpf", false, "qualify the cgroup BPF candidate using a separate privileged loader")
	flag.Parse()
	if *share == "" || *image == "" || *out == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 8*time.Minute)
	defer timeout()
	result := report{Version: 1, Time: time.Now().UTC(), Mode: "ordinary-masks", Mounts: map[string][]cruntime.DockerMount{}, Limitations: []string{"Finite fixed fixtures do not qualify Coop's full policy or other shares; production masks remain unchanged."}}
	if *bpf {
		result.Mode = "bpf-cgroup-candidate"
	}
	err := run(ctx, *share, *image, *bpf, &result)
	result.Complete = err == nil
	if err != nil {
		result.Error = err.Error()
	}
	data, marshalErr := json.MarshalIndent(result, "", "  ")
	if marshalErr == nil {
		marshalErr = os.WriteFile(*out, append(data, '\n'), 0600)
	}
	if err != nil || marshalErr != nil {
		fmt.Fprintln(os.Stderr, errors.Join(err, marshalErr))
		os.Exit(1)
	}
	fmt.Printf("Completed %s qualification; qualified=%t; evidence: %s\n", result.Mode, result.Qualified, *out)
}

var _ io.Writer = (*boundedBuffer)(nil)
