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
	"path/filepath"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
)

const (
	RuntimeObserveSocket = "/private/observe.sock"
	ObservationDirectory = "/observations"
	FinalObservationPath = ObservationDirectory + "/final.json"
	MaxSnapshotBytes     = 1 << 20
)

type RuntimeObservation struct {
	Version  int                  `json:"version"`
	Identity Identity             `json:"identity"`
	Network  networkview.Snapshot `json:"network"`
}

func (g *GuardRuntime) observation() RuntimeObservation {
	return RuntimeObservation{Version: 1, Identity: g.Identity, Network: g.collector.Snapshot()}
}

func (g *GuardRuntime) finishObservation(path string, prior error) error {
	return errors.Join(prior, writeFinalObservation(path, g.observation()))
}

func (g *GuardRuntime) serveObservation(ctx context.Context, path string, authenticate func(*net.UnixConn) bool) error {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return Failure("observation_unavailable")
	}
	defer listener.Close()
	if os.Chmod(path, 0600) != nil {
		return Failure("observation_unavailable")
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	slots := make(chan struct{}, 8)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return Failure("observation_unavailable")
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		workers.Go(func() {
			defer func() { <-slots; _ = conn.Close() }()
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			if conn.SetDeadline(time.Now().Add(ControlTimeout)) != nil || !authenticate(conn) {
				return
			}
			var request controlRequest
			if readControl(conn, &request) != nil || request.Version != 1 || request.Identity != g.Identity || request.Operation != "snapshot" || request.Lease != nil || request.AfterBoot != 0 {
				return
			}
			data, err := encodeObservation(g.observation())
			if err == nil {
				_ = writeAll(conn, data)
			}
		})
	}
}

func encodeObservation(value RuntimeObservation) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, Failure("observation_limit")
	}
	if len(data)+1 > MaxSnapshotBytes {
		// Retain terminal custody and source totals even when detail outgrows
		// the transport frame. Empty detail is explicitly omitted, not zero.
		projection := value.Network.Projection
		value.Network = value.Network.Project(true)
		value.Network.Projection = projection
		omitted := len(value.Network.Connections) + len(value.Network.Denials)
		value.Network.Connections, value.Network.Denials = nil, nil
		value.Network.Loss.DetailTruncated = true
		value.Network.Loss.Reasons = append(value.Network.Loss.Reasons, "snapshot_detail_limit")
		if count := value.Network.Loss.OmittedDetails; count != nil && !networkview.Add(count, uint64(omitted)) {
			value.Network.Loss.OmittedDetails = nil
		}
		data, err = json.Marshal(value)
		if err != nil || len(data)+1 > MaxSnapshotBytes {
			return nil, Failure("observation_limit")
		}
	}
	return append(data, '\n'), nil
}

func ReadObservation(reader io.Reader, identity Identity) (RuntimeObservation, error) {
	line, err := bufio.NewReaderSize(io.LimitReader(reader, MaxSnapshotBytes+1), MaxSnapshotBytes+1).ReadSlice('\n')
	if err != nil || len(line) > MaxSnapshotBytes {
		return RuntimeObservation{}, Failure("observation_invalid")
	}
	var value RuntimeObservation
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Version != 1 || value.Identity != identity ||
		value.Network.Version != networkview.Version || value.Network.RunID != identity.RunID || value.Network.Epoch != identity.Epoch || value.Network.PolicyFingerprint != identity.PolicyFingerprint {
		return RuntimeObservation{}, Failure("observation_invalid")
	}
	return value, nil
}

// ObserveRuntime is executed by the host in the trusted helper, never exposed
// as an agent command, TCP API or arbitrary target/path probe.
func ObserveRuntime(ctx context.Context, config LaunchConfig) (RuntimeObservation, error) {
	if config.Validate() != nil || verifyServiceRole("guard") != nil {
		return RuntimeObservation{}, Failure("gateway_configuration_invalid")
	}
	clock, err := OpenBootClock()
	if err != nil {
		return RuntimeObservation{}, err
	}
	return observeAt(ctx, RuntimeObserveSocket, config.identity(clock))
}

func observeAt(ctx context.Context, path string, identity Identity) (RuntimeObservation, error) {
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return RuntimeObservation{}, Failure("observation_unavailable")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if conn.SetDeadline(deadline) != nil || json.NewEncoder(conn).Encode(controlRequest{Identity: identity, Version: 1, Operation: "snapshot"}) != nil {
		return RuntimeObservation{}, Failure("observation_unavailable")
	}
	return ReadObservation(conn, identity)
}

// This dedicated volume is writable only by the trusted helper and survives
// its exit long enough for exact-owned host capture. It is never an agent mount.
func prepareObservationDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return Failure("observation_storage_unavailable")
	}
	for _, name := range []string{"final.json", ".final.tmp"} {
		if _, err := os.Lstat(filepath.Join(path, name)); !os.IsNotExist(err) {
			return Failure("observation_generation_used")
		}
	}
	probe := filepath.Join(path, ".write-check")
	file, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Failure("observation_storage_unavailable")
	}
	closeErr := file.Close()
	removeErr := os.Remove(probe)
	if closeErr != nil || removeErr != nil {
		return Failure("observation_storage_unavailable")
	}
	return nil
}

func writeFinalObservation(path string, value RuntimeObservation) error {
	data, err := encodeObservation(value)
	if err != nil {
		return err
	}
	tmp := filepath.Join(path, ".final.tmp")
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Failure("observation_storage_unavailable")
	}
	defer os.Remove(tmp)
	writeErr := writeAll(file, data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return Failure("observation_storage_unavailable")
	}
	if os.Link(tmp, filepath.Join(path, "final.json")) != nil {
		return Failure("observation_generation_used")
	}
	directory, err := os.Open(path)
	if err != nil {
		return Failure("observation_storage_unavailable")
	}
	defer directory.Close()
	if directory.Sync() != nil {
		return Failure("observation_storage_unavailable")
	}
	return nil
}
