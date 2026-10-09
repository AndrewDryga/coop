package nativedocker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

type nativeOnlineRequest struct {
	Args   []string
	Finish bool
}
type nativeOnlineReply struct{ Output, HoldID, Error string }
type nativeOnlineFixture struct {
	mu                      sync.Mutex
	endpoint, calls, boxEnv string
	serial                  uint64
	containers              map[string]runtime.DockerContainer
}

// The child CLI and daemon API share one test-owned inventory. Observations are
// derived from emitted create flags, rather than assuming a safe container.
func New(t *testing.T, calls, boxEnv string) runtime.Runtime {
	t.Helper()
	f := &nativeOnlineFixture{calls: calls, boxEnv: boxEnv, containers: map[string]runtime.DockerContainer{}}
	endpoint := dockersock.ServeHandler(t, http.HandlerFunc(f.serve))
	f.mu.Lock()
	f.endpoint = endpoint
	f.mu.Unlock()
	for _, key := range []string{"DOCKER_CONTEXT", "DOCKER_API_VERSION", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		t.Setenv(key, "")
	}
	t.Setenv("DOCKER_HOST", endpoint)
	path := filepath.Join(t.TempDir(), "docker")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nexport GORACE='atexit_sleep_ms=0'\nexec " + quote(os.Args[0]) + " -test.run=^TestNativeOnlineFixtureProcess$ -- " + quote(endpoint) + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: path}
}

func Process(t *testing.T) {
	index := slices.Index(os.Args, "--")
	if index < 0 {
		return
	}
	if len(os.Args) <= index+2 {
		os.Exit(91)
	}
	endpoint, args := os.Args[index+1], os.Args[index+2:]
	if len(args) > 0 && args[0] == "--config" {
		for _, key := range []string{"DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_API_VERSION", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
			if _, exists := os.LookupEnv(key); exists {
				os.Exit(92)
			}
		}
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(endpoint, "unix://"))
	}}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	call := func(value nativeOnlineRequest) nativeOnlineReply {
		data, err := json.Marshal(value)
		if err != nil {
			os.Exit(93)
		}
		response, err := client.Post("http://fixture/command", "application/json", bytes.NewReader(data))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(94)
		}
		var reply nativeOnlineReply
		err = json.NewDecoder(response.Body).Decode(&reply)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			os.Exit(95)
		}
		if reply.Error != "" {
			fmt.Fprintln(os.Stderr, reply.Error)
			os.Exit(1)
		}
		return reply
	}
	reply := call(nativeOnlineRequest{Args: args})
	fmt.Print(reply.Output)
	if reply.HoldID != "" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		call(nativeOnlineRequest{Args: []string{reply.HoldID}, Finish: true})
	}
	os.Exit(0) // no testing PASS text on the readiness stream
}

func (f *nativeOnlineFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/info" {
		_ = json.NewEncoder(w).Encode(dockersock.Info{ID: "fixture-daemon", OSType: "linux", Architecture: "amd64", ServerVersion: "29.1", KernelVersion: "fixture", SecurityOptions: []string{}})
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/command" {
		http.NotFound(w, r)
		return
	}
	var request nativeOnlineRequest
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request) != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	reply, err := f.command(request)
	if err != nil {
		reply.Error = err.Error()
	}
	_ = json.NewEncoder(w).Encode(reply)
}

func nativeOnlineJSON(value any) (nativeOnlineReply, error) {
	data, err := json.Marshal(value)
	return nativeOnlineReply{Output: string(data) + "\n"}, err
}

func (f *nativeOnlineFixture) command(request nativeOnlineRequest) (nativeOnlineReply, error) {
	fail := func() (nativeOnlineReply, error) {
		return nativeOnlineReply{}, fmt.Errorf("unsupported online fixture command: %q", request.Args)
	}
	if request.Finish {
		if len(request.Args) != 1 {
			return fail()
		}
		id := request.Args[0]
		value, present := f.containers[id]
		if !present {
			return nativeOnlineReply{}, nil
		}
		if value.AutoRemove {
			delete(f.containers, id)
		} else {
			value.State.Status, value.State.Running = "exited", false
			value.State.FinishedAt = time.Now().UTC()
			f.containers[id] = value
		}
		return nativeOnlineReply{}, nil
	}
	args := request.Args
	bound := false
	if len(args) >= 2 && args[0] == "--config" {
		entries, err := os.ReadDir(args[1])
		if err != nil || len(entries) != 0 {
			return fail()
		}
		args = args[2:]
	}
	if len(args) >= 2 && args[0] == "--host" {
		if args[1] != f.endpoint {
			return fail()
		}
		bound, args = true, args[2:]
	}
	if len(args) == 0 {
		return fail()
	}
	log, err := os.OpenFile(f.calls, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nativeOnlineReply{}, err
	}
	_, err = fmt.Fprintln(log, strings.Join(args, " "))
	err = errors.Join(err, log.Close())
	if err != nil {
		return nativeOnlineReply{}, err
	}
	image := "sha256:" + strings.Repeat("c", 64)
	find := func(target string) (runtime.DockerContainer, bool) {
		if value, ok := f.containers[target]; ok {
			return value, true
		}
		for _, value := range f.containers {
			if strings.TrimPrefix(value.Name, "/") == target {
				return value, true
			}
		}
		return runtime.DockerContainer{}, false
	}
	create := func(value runtime.DockerContainer) (runtime.DockerContainer, error) {
		if value.Name == "" {
			return value, errors.New("fixture container needs name")
		}
		if _, present := find(strings.TrimPrefix(value.Name, "/")); present {
			return value, errors.New("duplicate fixture container")
		}
		f.serial++
		sum := sha256.Sum256([]byte(f.endpoint + value.Name + strconv.FormatUint(f.serial, 10)))
		value.ID = hex.EncodeToString(sum[:])
		value.State = runtime.DockerContainerState{Status: "created"}
		f.containers[value.ID] = value
		return value, nil
	}
	switch args[0] {
	case "info":
		if len(args) > 1 {
			return nativeOnlineReply{Output: "linux/amd64\n"}, nil
		}
		return nativeOnlineJSON(map[string]string{"ID": "fixture-daemon"})
	case "context":
		if len(args) >= 2 && args[1] == "show" {
			return nativeOnlineReply{Output: "fixture\n"}, nil
		}
		if len(args) >= 2 && args[1] == "inspect" {
			return nativeOnlineJSON(map[string]any{"Host": f.endpoint, "SkipTLSVerify": false})
		}
	case "image":
		if len(args) == 3 && args[1] == "inspect" {
			return nativeOnlineJSON([]map[string]any{{"Id": image}})
		}
		if len(args) != 5 || args[1] != "inspect" || args[2] != "--format" {
			return fail()
		}
		switch {
		case strings.Contains(args[3], `"ID"`):
			return nativeOnlineJSON(map[string]any{"ID": image, "Labels": map[string]string{gatewayimage.BuildLabel: gatewayimage.Fingerprint()}})
		case args[3] == "{{.Id}}":
			return nativeOnlineReply{Output: image + "\n"}, nil
		case strings.Contains(args[3], gatewayimage.BuildLabel):
			return nativeOnlineReply{Output: gatewayimage.Fingerprint() + "\n"}, nil
		}
	case "container":
		if !bound || len(args) < 2 {
			return fail()
		}
		switch args[1] {
		case "create":
			value, _, rest, err := nativeOnlineObservation(args[2:])
			if err != nil || len(rest) != 2 || rest[0] != image || rest[1] != "native-broker" {
				return fail()
			}
			value.Image = rest[0]
			value, err = create(value)
			return nativeOnlineReply{Output: value.ID + "\n"}, err
		case "inspect":
			value, present := find(args[len(args)-1])
			if !present {
				return nativeOnlineReply{}, errors.New("fixture container absent")
			}
			return nativeOnlineJSON(value)
		case "start":
			if len(args) != 5 || args[2] != "--attach" || args[3] != "--interactive" {
				return fail()
			}
			id := args[4]
			value, present := f.containers[id]
			if !present || value.State.Status != "created" {
				return fail()
			}
			value.State = runtime.DockerContainerState{Status: "running", Running: true, StartedAt: time.Now().UTC()}
			f.containers[id] = value
			return nativeOnlineReply{Output: "ready\n", HoldID: id}, nil
		case "ls":
			return f.list(args[2:], true)
		case "rm":
			if len(args) != 4 || args[2] != "--force" {
				return fail()
			}
			if _, present := f.containers[args[3]]; !present {
				return fail()
			}
			delete(f.containers, args[3])
			return nativeOnlineReply{}, nil
		}
	case "ps":
		return f.list(args[1:], false)
	case "inspect":
		if len(args) != 4 || args[1] != "--format" || args[2] != "{{json .Mounts}}" {
			return fail()
		}
		value, present := find(args[3])
		if !present {
			return fail()
		}
		return nativeOnlineJSON(value.Mounts)
	case "rm":
		if len(args) != 3 || args[1] != "-f" {
			return fail()
		}
		if _, present := f.containers[args[2]]; !present {
			return fail()
		}
		delete(f.containers, args[2])
		return nativeOnlineReply{}, nil
	case "network", "volume":
		if len(args) >= 2 && args[1] == "ls" {
			return nativeOnlineReply{}, nil
		}
		return fail()
	case "run":
		value, options, rest, err := nativeOnlineObservation(args[1:])
		if err != nil || len(rest) == 0 {
			return fail()
		}
		value.Image = rest[0]
		if len(rest) == 2 && rest[0] == gatewayimage.Tag() && rest[1] == "broker" {
			value, err = create(value)
			if err != nil {
				return nativeOnlineReply{}, err
			}
			value.State = runtime.DockerContainerState{Status: "running", Running: true, StartedAt: time.Now().UTC()}
			f.containers[value.ID] = value
			return nativeOnlineReply{Output: "172.18.0.5\n", HoldID: value.ID}, nil
		}
		joined, ok := strings.CutPrefix(value.NetworkMode, "container:")
		owner, present := f.containers[joined]
		grader := value.NetworkMode == "none" && len(rest) >= 3 && rest[1] == "/bin/sh" && strings.HasPrefix(rest[2], "/coop-verifier/")
		if grader {
			for _, arg := range options {
				if strings.HasPrefix(arg, "COOP_PRIMARY=") {
					return fail()
				}
			}
			for _, mount := range value.Mounts {
				if strings.HasPrefix(mount.Destination, "/home/node/.") {
					return fail()
				}
			}
		}
		if ok && (!bound || !present || !owner.State.Running || !strings.HasPrefix(owner.Name, "/coop-native-")) {
			return nativeOnlineReply{}, errors.New("workload did not join exact running native owner")
		}
		for _, mount := range value.Mounts {
			if !ok && mount.Destination == "/run/coop-native-ca.pem" {
				return nativeOnlineReply{}, errors.New("brokered workload did not join native owner")
			}
			if grader && strings.HasPrefix(mount.Destination, "/coop-verifier") && mount.RW {
				return nativeOnlineReply{}, errors.New("grader received writable verifier")
			}
			for _, private := range owner.Mounts {
				if mount.Type == "bind" && private.Type == "bind" && pathContains(private.Source, mount.Source) {
					return nativeOnlineReply{}, errors.New("workload received native private mount")
				}
			}
		}
		for i := 0; i+1 < len(options); i++ {
			if options[i] == "--env-file" {
				data, err := os.ReadFile(options[i+1])
				if err != nil {
					return nativeOnlineReply{}, err
				}
				if err := os.WriteFile(f.boxEnv, data, 0600); err != nil {
					return nativeOnlineReply{}, err
				}
				i++
			}
		}
		return nativeOnlineReply{}, nil
	}
	return fail()
}

func (f *nativeOnlineFixture) list(args []string, objects bool) (nativeOnlineReply, error) {
	var output strings.Builder
	var ids []string
	for id := range f.containers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		value := f.containers[id]
		match := slices.Contains(args, "-a") || slices.Contains(args, "--all") || value.State.Running
		for i := 0; i < len(args); i++ {
			if args[i] != "--filter" {
				continue
			}
			i++
			if i == len(args) {
				return nativeOnlineReply{}, errors.New("incomplete fixture filter")
			}
			kind, selected, _ := strings.Cut(args[i], "=")
			switch kind {
			case "id":
				match = match && value.ID == selected
			case "name":
				match = match && strings.TrimPrefix(value.Name, "/") == selected
			case "label":
				key, want, hasValue := strings.Cut(selected, "=")
				got, exists := value.Labels[key]
				match = match && exists && (!hasValue || got == want)
			default:
				return nativeOnlineReply{}, errors.New("unsupported fixture filter")
			}
		}
		if match {
			if objects {
				data, _ := json.Marshal(map[string]string{"name": strings.TrimPrefix(value.Name, "/"), "id": id})
				output.Write(data)
				output.WriteByte('\n')
			} else {
				output.WriteString(id + "\n")
			}
		}
	}
	return nativeOnlineReply{Output: output.String()}, nil
}

func nativeOnlineObservation(args []string) (runtime.DockerContainer, []string, []string, error) {
	value := runtime.DockerContainer{Labels: map[string]string{}, Tmpfs: map[string]string{}}
	var options, rest []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			rest = args[i:]
			break
		}
		flag, inline, hasInline := strings.Cut(args[i], "=")
		switch flag {
		case "--rm", "--init", "--read-only", "--privileged", "-i", "-t", "-it":
			if hasInline {
				return value, nil, nil, errors.New("invalid fixture boolean")
			}
			options = append(options, flag)
			switch flag {
			case "--rm":
				value.AutoRemove = true
			case "--read-only":
				value.ReadonlyRootfs = true
			case "--privileged":
				value.Privileged = true
			case "-i":
				value.OpenStdin = true
			case "-t":
				value.Tty = true
			case "-it":
				value.OpenStdin, value.Tty = true, true
			}
			continue
		}
		if !slices.Contains([]string{"--pull", "--restart", "--name", "--user", "--network", "--cap-drop", "--cap-add", "--security-opt", "--memory", "--cpus", "--pids-limit", "--mount", "-v", "--tmpfs", "--label", "-e", "--env-file", "-w", "--workdir", "--hostname", "--add-host", "--dns", "--dns-search", "--dns-option", "--entrypoint", "-p", "--log-driver", "--log-opt", "--pid", "--ipc", "--userns", "--cgroupns"}, flag) {
			return value, nil, nil, fmt.Errorf("unsupported fixture option %s", flag)
		}
		selected := inline
		if !hasInline {
			i++
			if i == len(args) {
				return value, nil, nil, errors.New("incomplete fixture option")
			}
			selected = args[i]
		}
		options = append(options, flag, selected)
		switch flag {
		case "--name":
			value.Name = "/" + selected
		case "--user":
			value.User = selected
		case "--network":
			value.NetworkMode = selected
		case "--restart":
			value.RestartPolicy = selected
		case "--cap-drop":
			value.CapDrop = append(value.CapDrop, selected)
		case "--cap-add":
			value.CapAdd = append(value.CapAdd, selected)
		case "--security-opt":
			value.SecurityOpt = append(value.SecurityOpt, selected)
		case "--pid":
			value.PidMode = selected
		case "--ipc":
			value.IpcMode = selected
		case "--userns":
			value.UsernsMode = selected
		case "--cgroupns":
			value.CgroupnsMode = selected
		case "--label":
			key, v, _ := strings.Cut(selected, "=")
			value.Labels[key] = v
		case "--tmpfs":
			dest, constraints, _ := strings.Cut(selected, ":")
			value.Tmpfs[dest] = constraints
		}
	}
	plan, err := fixtureMountPlan(options)
	if err != nil {
		return value, nil, nil, err
	}
	for _, mount := range plan {
		if mount.Type != "tmpfs" {
			value.Mounts = append(value.Mounts, mount)
		}
	}
	slices.SortFunc(value.Mounts, func(a, b runtime.DockerMount) int { return strings.Compare(a.Destination, b.Destination) })
	return value, options, rest, nil
}

func fixtureMountPlan(options []string) (map[string]runtime.DockerMount, error) {
	result := map[string]runtime.DockerMount{}
	for i := 0; i < len(options); i++ {
		kind := options[i]
		if kind != "-v" && kind != "--mount" && kind != "--tmpfs" {
			continue
		}
		i++
		if i == len(options) {
			return nil, errors.New("incomplete restricted mount plan")
		}
		mount := runtime.DockerMount{RW: true}
		switch kind {
		case "-v":
			parts := strings.Split(options[i], ":")
			if len(parts) != 2 && len(parts) != 3 || len(parts) == 3 && parts[2] != "ro" {
				return nil, errors.New("ambiguous restricted mount plan")
			}
			mount.Source, mount.Destination, mount.Type = parts[0], parts[1], "bind"
			mount.RW = len(parts) == 2
			if !filepath.IsAbs(mount.Source) {
				mount.Type, mount.Name = "volume", mount.Source
				mount.Source = ""
			}
		case "--mount":
			fields, err := csv.NewReader(strings.NewReader(options[i])).Read()
			if err != nil {
				return nil, errors.New("invalid restricted mount descriptor")
			}
			for _, field := range fields {
				key, value, _ := strings.Cut(field, "=")
				switch key {
				case "type":
					mount.Type = value
				case "source":
					mount.Source = value
				case "target":
					mount.Destination = value
				case "readonly":
					mount.RW = false
				default:
					return nil, errors.New("unknown restricted mount field")
				}
			}
			if mount.Type == "volume" {
				mount.Name, mount.Source = mount.Source, ""
			}
		case "--tmpfs":
			mount.Type = "tmpfs"
			mount.Destination, _, _ = strings.Cut(options[i], ":")
		}
		if !filepath.IsAbs(mount.Destination) {
			return nil, errors.New("restricted mount destination is not absolute")
		}
		if _, duplicate := result[mount.Destination]; duplicate {
			return nil, errors.New("restricted mount destination occurs twice")
		}
		result[mount.Destination] = mount
	}
	return result, nil
}

func pathContains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
