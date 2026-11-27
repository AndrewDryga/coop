package box

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// ForwardService uses an immutable release helper. Entering the target's network
// namespace also works when the connector and its Docker daemon run in separate containers.
func (p ControllerProject) ForwardService(ctx context.Context, rt runtime.Runtime, name, container string, port int, input io.Reader, output io.Writer, halfClose func() error) (result error) {
	bound, services, err := p.catalog(ctx, rt)
	if err != nil {
		return err
	}
	permitted := slices.ContainsFunc(services, func(s ControllerService) bool { return s.Name == name && slices.Contains(s.Ports, port) })
	if !permitted {
		return errors.New("service port is not declared")
	}
	labels := p.labels(name)
	confirm := func() error {
		observed, found, err := bound.ObserveServiceContainer(ctx, labels)
		if err != nil || !found || observed.ID != container || !serviceContainerUp(observed) {
			return errors.New("approved service is no longer running")
		}
		return nil
	}
	if err := confirm(); err != nil {
		return err
	}
	endpoint, daemon := bound.ComposeBinding()
	docker, err := runtime.BindDockerForSetup(ctx, bound, endpoint, daemon)
	if err != nil {
		return err
	}
	defer docker.Close()
	image, marks, err := docker.Image(ctx, gatewayimage.Tag())
	if err != nil || marks[gatewayimage.BuildLabel] != gatewayimage.Fingerprint() {
		return errors.New("service relay requires this worker build's trusted network image; run coop net setup")
	}
	ref := runtime.DockerRef{Name: "coop-service-" + randid.Hex(12), Labels: map[string]string{controllerServiceJobLabel: p.JobDigest, composeProjectLabel: ComposeProject(p.Workspace), "coop.service.relay": container}}
	namespace := "container:" + container
	options := []string{"-i", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65532:65532", "--read-only", "--log-driver", "none", "--cpus", "0.25", "--memory", "32m", "--pids-limit", "16", "--network", namespace}
	ref.ID, err = docker.CreateContainer(ctx, runtime.DockerCreate{Ref: ref, Image: image, Options: options, Command: []string{"service-relay", strconv.Itoa(port)}})
	if !runtime.DockerCreateLeftNothing(err) {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if ref.ID == "" {
				v, found, inspectErr := docker.InspectContainer(cleanup, ref)
				if inspectErr != nil || !found {
					result = errors.Join(result, errors.New("service relay creation could not be reconciled"), inspectErr)
					return
				}
				ref.ID = v.ID
			}
			result = errors.Join(result, docker.RemoveContainer(cleanup, ref))
		}()
	}
	if err != nil {
		return err
	}
	if err := confirm(); err != nil {
		return err
	}
	attached, writer := io.Pipe()
	decoded := make(chan error, 1)
	go func() {
		parseErr := networkgateway.ReadServiceFrames(output, halfClose, attached)
		_ = attached.CloseWithError(parseErr)
		decoded <- parseErr
	}()
	code, err := docker.StartAttached(ctx, ref, input, writer, io.Discard, nil)
	_ = writer.CloseWithError(err)
	if parseErr := <-decoded; err != nil || parseErr != nil || code != 0 {
		return errors.New("service relay connection ended unsuccessfully")
	}
	return nil
}
