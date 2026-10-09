package box

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// An open run has the same private broker namespace as a filtered run, but no
// egress policy. The owner is capless, holds no repository mounts, and exposes
// only loopback; the workload joins its exact, inspected container identity.
type nativeOpenOwner struct {
	docker *runtime.Docker
	ref    runtime.DockerRef
	input  *io.PipeWriter
	cancel context.CancelFunc
	done   chan error
}

func startNativeOpen(ctx context.Context, rt runtime.Runtime, run *nativeRun, network string, options, owner []string, building func()) (*nativeOpenOwner, error) {
	if run == nil || run.dir == "" {
		return nil, errors.New("native broker private state is unavailable")
	}
	if network == "" {
		network = "bridge"
	}
	if network == "host" || network == "none" || strings.HasPrefix(network, "container:") {
		return nil, errors.New("native credentials require a private run namespace; use an ordinary bridge or services network")
	}
	tag, err := ensureOpenBrokerImage(ctx, rt, building)
	if err != nil {
		return nil, err
	}
	docker, err := runtime.BindDocker(ctx, rt, "", "")
	if err != nil {
		return nil, err
	}
	labels := map[string]string{LabelKey: LabelBroker, labelBrokerID: run.runID}
	for i := 0; i+1 < len(owner); i++ {
		if owner[i] == "--label" {
			key, value, ok := strings.Cut(owner[i+1], "=")
			if ok {
				labels[key] = value
			}
			i++
		}
	}
	labels[LabelKey] = LabelBroker
	result := &nativeOpenOwner{docker: docker, ref: runtime.DockerRef{Name: "coop-native-" + run.runID[:16], Labels: labels}, done: make(chan error, 1)}
	image, _, err := docker.Image(ctx, tag)
	if err != nil {
		return result, err
	}
	args := []string{"-i", "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
		"--memory", "256m", "--pids-limit", "64", "--network", network,
		"--mount", networkMount("bind", run.dir, networkgateway.NativePrivateDirectory, true)}
	args = append(args, options...)
	if hostname := run.hostname(); hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	id, err := docker.CreateContainer(ctx, runtime.DockerCreate{Ref: result.ref, Image: image, Options: args, Command: []string{"native-broker"}})
	result.ref.ID = id
	if err != nil {
		return result, err
	}
	observed, present, err := docker.InspectContainer(ctx, result.ref)
	if err != nil || !present || observed.Image != image || observed.User != "65532:65532" || observed.NetworkMode != network ||
		observed.Privileged || !observed.ReadonlyRootfs || len(observed.CapAdd) != 0 || !slices.Equal(observed.CapDrop, []string{"ALL"}) ||
		!slices.ContainsFunc(observed.SecurityOpt, func(s string) bool { return s == "no-new-privileges" || s == "no-new-privileges=true" }) ||
		observed.PidMode != "" || observed.UsernsMode != "" || observed.RestartPolicy != "no" {
		return result, errors.Join(errors.New("native broker namespace security state differs from its launch"), err)
	}
	if err := verifyNetworkMounts(observed.Mounts, observed.Tmpfs, args); err != nil {
		return result, err
	}
	input, writer := io.Pipe()
	output, stdout := io.Pipe()
	result.input = writer
	lifetime, cancel := context.WithCancel(ctx)
	result.cancel = cancel
	stderr := &tailBuffer{max: 8 << 10}
	go func() {
		defer input.Close()
		_, err := docker.StartAttached(lifetime, result.ref, input, stdout, stderr, nil)
		_ = stdout.Close()
		result.done <- err
	}()
	ready := make(chan bool, 1)
	go func() {
		line, err := bufio.NewReader(output).ReadString('\n')
		ready <- err == nil && line == "ready\n"
		_ = output.Close()
	}()
	timer := time.NewTimer(openBrokerReadyTimeout)
	defer timer.Stop()
	select {
	case ok := <-ready:
		if !ok {
			return result, errors.New("native broker did not report readiness")
		}
		current, present, err := docker.InspectContainer(ctx, result.ref)
		if err != nil || !present || !current.State.Running || current.NetworkMode != network {
			return result, errors.New("native broker namespace is not running")
		}
		return result, nil
	case <-timer.C:
		return result, errors.New("native broker readiness timed out")
	}
}

func (o *nativeOpenOwner) close() error {
	if o == nil {
		return nil
	}
	if o.input != nil {
		_ = o.input.Close()
	}
	if o.cancel != nil {
		o.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
	defer cancel()
	var err error
	if o.ref.ID == "" {
		value, present, inspectErr := o.docker.InspectContainer(ctx, o.ref)
		if inspectErr != nil {
			return inspectErr
		}
		if present {
			o.ref.ID = value.ID
		}
	}
	if o.ref.ID != "" {
		err = o.docker.RemoveContainer(ctx, o.ref)
	}
	if o.cancel != nil {
		select {
		case <-o.done:
		case <-ctx.Done():
			err = errors.Join(err, ctx.Err())
		}
	}
	return errors.Join(err, o.docker.Close())
}

// Namespace-level options belong to the owner, never the joining workload.
// Network mode is selected separately; all remaining workload options retain
// their order and values. Unknown Docker options stay subject to normal checks.
func nativeNetworkOptions(args []string) (workload, owner []string, network string, err error) {
	for i := 0; i < len(args); i++ {
		flag, value, inline := strings.Cut(args[i], "=")
		switch flag {
		case "--network", "--net", "--add-host", "--dns", "--dns-search", "--dns-option", "--hostname", "-h", "--publish", "-p":
			if !inline {
				if i+1 >= len(args) {
					return nil, nil, "", fmt.Errorf("%s needs a value", flag)
				}
				i++
				value = args[i]
			}
			if flag == "--network" || flag == "--net" {
				network = value
				continue
			}
			if flag == "-h" {
				flag = "--hostname"
			}
			if flag == "--publish" {
				flag = "-p"
			}
			owner = append(owner, flag, value)
		case "-P", "--publish-all":
			return nil, nil, "", errors.New("native broker namespace requires explicit published ports")
		default:
			workload = append(workload, args[i])
		}
	}
	return workload, owner, network, nil
}
