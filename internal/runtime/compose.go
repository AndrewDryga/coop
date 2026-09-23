package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FreezeCompose records the local daemon selected for one service operation.
// Every subsequent Compose command rechecks that exact daemon, even if the
// operator changes Docker contexts while a terminal approval is open.
func (r Runtime) FreezeCompose(ctx context.Context) (Runtime, error) {
	if r.kind() != runtimeDocker {
		return r, nil
	}
	docker, err := InspectDocker(ctx, r)
	if err != nil {
		return Runtime{}, err
	}
	defer docker.Close()
	r.composeEndpoint, r.composeDaemon = docker.Endpoint(), docker.Info().ID
	return r, nil
}

func (r Runtime) ComposeBinding() (endpoint, daemonID string) {
	return r.composeEndpoint, r.composeDaemon
}

// ComposeImageID inspects an image on the daemon frozen for this service operation.
// It never pulls; callers decide explicitly whether a terminal review may do so.
func (r Runtime) ComposeImageID(ctx context.Context, image string) (string, error) {
	if r.composeEndpoint == "" || r.composeDaemon == "" {
		return "", errors.New("Docker daemon must be frozen before inspecting a service image")
	}
	docker, err := bindDocker(ctx, r, r.composeEndpoint, r.composeDaemon, false)
	if err != nil {
		return "", err
	}
	defer docker.Close()
	id, _, err := docker.Image(ctx, image)
	return id, err
}

// PullComposeImage is used only before a new terminal service approval. The
// reference is an argument, never a Docker option or shell fragment.
func (r Runtime) PullComposeImage(image string) error {
	if r.composeEndpoint == "" || r.composeDaemon == "" || !dockerToken(image, 512) || strings.HasPrefix(image, "-") {
		return errors.New("invalid service image reference or unfrozen Docker daemon")
	}
	code, err := r.RunCompose(nil, io.Discard, io.Discard, "pull", image)
	if err != nil {
		return err
	}
	if code != 0 {
		return errors.New("Docker image pull failed")
	}
	return nil
}

// RunCompose uses a private Docker config containing registry authentication
// but never the host's proxy injection rules. It pins a frozen operation to its
// inspected local daemon. Other runtime dialects retain their existing runner.
func (r Runtime) RunCompose(stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	if r.kind() != runtimeDocker {
		return r.Run(stdin, stdout, stderr, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var docker *Docker
	var err error
	if r.composeEndpoint == "" {
		docker, err = InspectDocker(ctx, r)
	} else {
		docker, err = bindDocker(ctx, r, r.composeEndpoint, r.composeDaemon, false)
	}
	if err != nil {
		return -1, err
	}
	defer docker.Close()
	config, cleanup, err := privateComposeClientConfig()
	if err != nil {
		return -1, err
	}
	defer cleanup()
	authEnv, err := composeAuthEnvironment()
	if err != nil {
		return -1, err
	}
	argv := append([]string{"--config", config, "--host", docker.Endpoint()}, args...)
	cmd := exec.Command(docker.Binary(), argv...)
	cmd.Env = docker.env
	if authEnv != "" {
		cmd.Env = append(cmd.Env, authEnv)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return exitCode(docker.Binary(), cmd.Run())
}

// Docker's environment-only registry auth takes precedence over config.json and credential
// helpers. Keep that exact CLI behavior for Compose, but accept only bounded auth data and never
// pass other DOCKER_* settings or proxy configuration into the service operation.
func composeAuthEnvironment() (string, error) {
	value := os.Getenv("DOCKER_AUTH_CONFIG")
	if value == "" {
		return "", nil
	}
	if len(value) > 1<<20 {
		return "", errors.New("docker registry auth environment exceeds 1 MiB")
	}
	var config struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return "", errors.New("docker registry auth environment is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", errors.New("docker registry auth environment has extra data")
	}
	for registry, auth := range config.Auths {
		decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
		if registry == "" || err != nil || !strings.Contains(string(decoded), ":") {
			return "", errors.New("docker registry auth environment has an invalid entry")
		}
	}
	return "DOCKER_AUTH_CONFIG=" + value, nil
}

func privateComposeClientConfig() (string, func(), error) {
	root, err := os.MkdirTemp("", "coop-compose-client-")
	if err != nil {
		return "", nil, err
	}
	var created []string
	cleanup := func() {
		for i := len(created) - 1; i >= 0; i-- {
			_ = os.Remove(created[i])
		}
		_ = os.Remove(root)
	}
	original := os.Getenv("DOCKER_CONFIG")
	if original == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			cleanup()
			return "", nil, homeErr
		}
		original = filepath.Join(home, ".docker")
	}
	data, err := os.ReadFile(filepath.Join(original, "config.json"))
	if err == nil {
		if len(data) > 1<<20 {
			cleanup()
			return "", nil, errors.New("Docker client config exceeds 1 MiB")
		}
		var source map[string]json.RawMessage
		if err := json.Unmarshal(data, &source); err != nil {
			cleanup()
			return "", nil, errors.New("Docker client config is invalid")
		}
		selected := map[string]json.RawMessage{}
		for _, key := range []string{"auths", "credsStore", "credHelpers"} {
			if value, ok := source[key]; ok {
				selected[key] = value
			}
		}
		filtered, err := json.Marshal(selected)
		if err != nil {
			cleanup()
			return "", nil, err
		}
		path := filepath.Join(root, "config.json")
		if err := os.WriteFile(path, filtered, 0o600); err != nil {
			cleanup()
			return "", nil, err
		}
		created = append(created, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		cleanup()
		return "", nil, err
	}
	plugins := filepath.Join(original, "cli-plugins")
	if info, err := os.Stat(plugins); err == nil && info.IsDir() {
		link := filepath.Join(root, "cli-plugins")
		if err := os.Symlink(plugins, link); err != nil {
			cleanup()
			return "", nil, err
		}
		created = append(created, link)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		cleanup()
		return "", nil, err
	}
	return root, cleanup, nil
}
