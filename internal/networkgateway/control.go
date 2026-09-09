package networkgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const (
	ControlTimeout    = 2 * time.Second
	HeartbeatInterval = time.Second
	HeartbeatTimeout  = 4 * time.Second
	GuardStartTimeout = 30 * time.Second
	maxControlBytes   = 4096
	maxControlClients = 64
)

type controlRequest struct {
	Identity  Identity    `json:"identity"`
	Version   int         `json:"version"`
	Operation string      `json:"operation"`
	Lease     *Lease      `json:"lease,omitempty"`
	AfterBoot BootInstant `json:"after_boot,omitempty"`
}

type controlReply struct {
	Identity   Identity      `json:"identity"`
	Version    int           `json:"version"`
	Ready      bool          `json:"ready"`
	Reason     string        `json:"reason,omitempty"`
	ValidUntil BootInstant   `json:"valid_until_boot_ns,omitempty"`
	Kernel     *KernelSample `json:"kernel,omitempty"`
}

// ServeControl exposes no policy mutation, path, command or arbitrary address
// operation. The private filesystem socket and kernel UID check are independent
// of the untrusted shared network namespace. The caller owns its private parent.
func (c *Controller) ServeControl(ctx context.Context, path string) error {
	return c.serveControl(ctx, path, servicePeer)
}

func (c *Controller) serveControl(ctx context.Context, path string, authenticate func(*net.UnixConn) bool) (result error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	// Closing admission blocks already-established forwarding too. It is a
	// one-way kernel transition, not merely expiry of new-connection leases.
	defer func() {
		cancel()
		shutdown, done := context.WithTimeout(context.Background(), ControlTimeout)
		defer done()
		if err := c.CloseAdmission(shutdown); err != nil {
			result = Failure("enforcement_shutdown_unconfirmed")
		}
		workers.Wait()
	}()
	leaseListener, err := controlListener(path)
	if err != nil {
		return err
	}
	defer leaseListener.Close()
	healthListener, err := controlListener(path + ".health")
	if err != nil {
		return err
	}
	defer healthListener.Close()
	observeListener, err := controlListener(path + ".observe")
	if err != nil {
		return err
	}
	defer observeListener.Close()
	stop := context.AfterFunc(ctx, func() { _ = leaseListener.Close(); _ = healthListener.Close(); _ = observeListener.Close() })
	defer stop()
	var heartbeatMu sync.Mutex
	deadline := c.now().Add(GuardStartTimeout)
	heartbeat := func() { heartbeatMu.Lock(); deadline = c.now().Add(HeartbeatTimeout); heartbeatMu.Unlock() }
	failed := make(chan error, 3)
	workers.Go(func() { failed <- c.controlConnections(ctx, leaseListener, "lease", authenticate, heartbeat) })
	workers.Go(func() { failed <- c.controlConnections(ctx, healthListener, "health", authenticate, heartbeat) })
	workers.Go(func() { _ = c.controlConnections(ctx, observeListener, "observe", authenticate, heartbeat) })
	workers.Go(func() { c.kernel.run(ctx, c.clock) })
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Failure("controller_stopped")
		case err := <-failed:
			return err
		case <-ticker.C:
			heartbeatMu.Lock()
			expired := !c.now().Before(deadline)
			heartbeatMu.Unlock()
			if expired || !c.Ready() {
				return Failure("controller_stopped")
			}
		}
	}
}

func controlListener(path string) (*net.UnixListener, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, Failure("controller_socket_unavailable")
	}
	if err := os.Chmod(path, 0660); err != nil {
		_ = listener.Close()
		return nil, Failure("controller_socket_unavailable")
	}
	return listener, nil
}

func (c *Controller) controlConnections(ctx context.Context, listener *net.UnixListener, endpoint string, authenticate func(*net.UnixConn) bool, heartbeat func()) error {
	// Slow frames and lease application have separate bounded resources from
	// liveness. Agent-driven connection bursts cannot masquerade as guard death.
	capacity := maxControlClients
	if endpoint != "lease" {
		capacity = 8
	}
	slots := make(chan struct{}, capacity)
	updates := make(chan struct{}, 1)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return Failure("controller_stopped")
			}
			return Failure("controller_socket_unavailable")
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		workers.Go(func() {
			defer func() { <-slots; _ = conn.Close() }()
			requestCtx, cancel := context.WithTimeout(ctx, ControlTimeout)
			defer cancel()
			deadline, _ := requestCtx.Deadline()
			if err := conn.SetDeadline(deadline); err != nil || !authenticate(conn) {
				return
			}
			var request controlRequest
			if err := readControl(conn, &request); err != nil {
				return
			}
			reply := controlReply{Identity: c.identity, Version: 1, Reason: "controller_request_refused"}
			if request.Version == 1 && request.Identity == c.identity {
				switch {
				case endpoint == "health" && request.Operation == "heartbeat" && request.Lease == nil && request.AfterBoot == 0:
					if c.Ready() {
						heartbeat()
						reply.Ready, reply.Reason = true, ""
					}
				case endpoint == "health" && request.Operation == "ready" && request.Lease == nil && request.AfterBoot == 0:
					reply.Ready = c.Ready()
					if reply.Ready {
						reply.Reason = ""
					}
				case endpoint == "observe" && request.Operation == "counters" && request.Lease == nil:
					sample := c.kernel.snapshot()
					if request.AfterBoot.Valid() {
						if request.AfterBoot.After(c.now()) {
							break
						}
						sample = c.kernel.after(requestCtx, request.AfterBoot)
					}
					sample.EnforcerReady = c.Ready()
					reply.Ready, reply.Reason, reply.Kernel = true, "", &sample
				case endpoint == "lease" && request.Operation == "lease" && request.Lease != nil && request.AfterBoot == 0:
					select {
					case updates <- struct{}{}:
					default:
						reply.Reason = "gateway_lease_capacity"
						_ = json.NewEncoder(conn).Encode(reply)
						return
					}
					until, err := c.Admit(requestCtx, *request.Lease)
					<-updates // only the kernel transaction owns the slot, not reply I/O
					if err != nil {
						var reason Failure
						if errors.As(err, &reason) {
							reply.Reason = string(reason)
						}
					} else {
						reply.Ready, reply.Reason = true, ""
						reply.ValidUntil = until
					}
				}
			}
			_ = json.NewEncoder(conn).Encode(reply)
		})
	}
}

// ControllerClient uses one bounded request per filesystem-socket connection.
// There is no TCP listener, proxy environment, DNS or caller-selected URL.
type ControllerClient struct {
	Path     string
	Identity Identity
	Clock    *BootClock
}

func (c ControllerClient) Admit(ctx context.Context, lease Lease) (BootInstant, error) {
	return c.call(ctx, controlRequest{Version: 1, Operation: "lease", Lease: &lease})
}

func (c ControllerClient) Ready(ctx context.Context) error {
	_, err := c.call(ctx, controlRequest{Version: 1, Operation: "ready"})
	return err
}

// Heartbeat is independent of host/Responder observation traffic. The guard
// must stop forwarding as soon as this loop returns, including controller EOF.
func (c ControllerClient) Heartbeat(ctx context.Context) error {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		if _, err := c.call(ctx, controlRequest{Version: 1, Operation: "heartbeat"}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c ControllerClient) call(ctx context.Context, request controlRequest) (BootInstant, error) {
	path := c.Path
	if request.Operation == "heartbeat" || request.Operation == "ready" {
		path += ".health"
	}
	return c.callPath(ctx, request, path)
}

func (c ControllerClient) callPath(ctx context.Context, request controlRequest, path string) (BootInstant, error) {
	reply, err := c.exchange(ctx, request, path)
	return reply.ValidUntil, err
}

func (c ControllerClient) Counters(ctx context.Context) (KernelSample, error) {
	return c.countersAfter(ctx, 0)
}

func (c ControllerClient) countersAfter(ctx context.Context, cutoff BootInstant) (KernelSample, error) {
	reply, err := c.exchange(ctx, controlRequest{Version: 1, Operation: "counters", AfterBoot: cutoff}, c.Path+".observe")
	if err != nil || reply.Kernel == nil {
		return KernelSample{}, Failure("kernel_counters_unavailable")
	}
	if cutoff.Valid() && (!reply.Kernel.StartedBoot.Valid() || reply.Kernel.StartedBoot.Before(cutoff)) {
		return KernelSample{}, Failure("kernel_terminal_sample_unavailable")
	}
	return *reply.Kernel, nil
}

func (c ControllerClient) exchange(ctx context.Context, request controlRequest, path string) (controlReply, error) {
	var empty controlReply
	if !c.Identity.Valid() || c.Clock.Domain() != c.Identity.Clock {
		return empty, Failure("enforcement_unavailable")
	}
	request.Identity = c.Identity
	started := c.Clock.instant()
	if !started.Valid() {
		return empty, Failure("enforcement_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return empty, Failure("enforcement_unavailable")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return empty, Failure("enforcement_unavailable")
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return empty, Failure("enforcement_unavailable")
	}
	var reply controlReply
	if err := readControl(conn, &reply); err != nil || reply.Version != 1 || reply.Identity != c.Identity {
		return empty, Failure("enforcement_unavailable")
	}
	if !reply.Ready {
		// Replies are private trusted data, but even a damaged peer cannot
		// inject arbitrary terminal text through an error channel.
		switch reply.Reason {
		case "gateway_lease_refused", "dns_ttl_expired", "gateway_lease_capacity":
			return empty, Failure(reply.Reason)
		default:
			return empty, Failure("enforcement_unavailable")
		}
	}
	completed := c.Clock.instant()
	if !completed.Valid() || completed.Before(started) || completed.Sub(started) > ControlTimeout {
		return empty, Failure("enforcement_unavailable")
	}
	if request.Operation == "lease" && (!completed.Before(reply.ValidUntil) || reply.ValidUntil.After(request.Lease.Expires)) {
		return empty, Failure("dns_ttl_expired")
	}
	return reply, nil
}

func readControl(reader io.Reader, target any) error {
	line, err := bufio.NewReaderSize(io.LimitReader(reader, maxControlBytes+1), maxControlBytes+1).ReadSlice('\n')
	if err != nil || len(line) > maxControlBytes {
		return Failure("controller_message_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return Failure("controller_message_invalid")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Failure("controller_message_invalid")
	}
	return nil
}
