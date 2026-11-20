package sessionsvc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"io"
	"os"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// The agent can report on its own work. Nothing in the writable output area
// grants authority. Keep the open file and import only frozen names and states.
const peerPointer = ".active-preset-turn"

type presetReport struct {
	marker *os.File
	file   *os.File
	name   string
}

func openPresetReport(dir *sessionOutputDirectory) (*presetReport, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	name := ".peer-" + hex.EncodeToString(nonce[:])
	f, err := dir.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	pointer := dir.name + " " + name
	// Rename a fresh file so a warm wrapper can see the entire new identity.
	tmp := ".peer-pointer-" + hex.EncodeToString(nonce[:])
	marker, err := dir.parent.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, err = io.WriteString(marker, pointer+"\n")
		if err == nil {
			err = dir.parent.Rename(tmp, peerPointer)
		}
		if err != nil {
			_ = marker.Close()
			_ = dir.parent.Remove(tmp)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &presetReport{file: f, name: name, marker: marker}, nil
}

func (book *presetReport) current(dir *sessionOutputDirectory) bool {
	expected := dir.name + " " + book.name + "\n"
	markerInfo, err := book.marker.Stat()
	named, pathErr := dir.parent.Lstat(peerPointer)
	if err != nil || pathErr != nil || !os.SameFile(markerInfo, named) || markerInfo.Size() != int64(len(expected)) {
		return false
	}
	data := make([]byte, len(expected))
	_, err = book.marker.ReadAt(data, 0)
	return err == nil && string(data) == expected
}

type peerCounts struct {
	status   string
	failures int
	calls    map[string]string
}

func readPresetReport(dir *sessionOutputDirectory, book *presetReport, roles []workerproto.JobPresetRole, finished bool) map[string]*peerCounts {
	peers := map[string]*peerCounts{}
	for _, role := range roles {
		peers[role.Name] = &peerCounts{status: "unknown", calls: map[string]string{}}
	}
	if book == nil || !dir.bindingIntact() || !book.current(dir) {
		return peers
	}
	opened, err := book.file.Stat()
	named, namedErr := dir.root.Lstat(book.name)
	if err != nil || namedErr != nil || !os.SameFile(opened, named) || !named.Mode().IsRegular() {
		return peers
	}
	const maxBytes = 64 << 10
	data, err := io.ReadAll(io.LimitReader(book.file, maxBytes+1))
	if err != nil || len(data) > maxBytes || (len(data) > 0 && data[len(data)-1] != '\n') {
		return peers
	}
	for _, row := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if row == "" {
			continue
		}
		parts := strings.Fields(row)
		if len(parts) != 3 {
			return peers
		}
		peer := peers[parts[0]]
		if peer == nil {
			return peers
		}
		if _, err := strconv.ParseUint(parts[1], 10, 32); err != nil {
			return peers
		}
		last := peer.calls[parts[1]]
		switch parts[2] {
		case "started":
			if last != "" {
				return peers
			}
		case "success", "failed":
			if last == "" {
				return peers
			}
			if parts[2] == "failed" {
				peer.failures++
			}
		default:
			return peers
		}
		peer.calls[parts[1]] = parts[2]
	}
	for _, role := range roles {
		p := peers[role.Name]
		if role.Mode == "native" {
			continue
		}
		if len(p.calls) == 0 && finished {
			p.status = "not_called"
			continue
		}
		answered, failed, pending := false, false, false
		for _, outcome := range p.calls {
			switch outcome {
			case "success":
				answered = true
			case "failed":
				failed = true
			default:
				pending = true
			}
		}
		switch {
		case pending:
			p.status = "unknown"
		case answered:
			p.status = "answered"
		case failed:
			p.status = "failed"
		}
	}
	return peers
}

func (r *sessionTurnRunner) startPresetHistory(ctx context.Context, bound session.Session, leased session.Turn, dir *sessionOutputDirectory) func(bool) {
	if !sessionHasJobField(bound, "preset") {
		return nil
	}
	job, err := sessionJobSpec(bound)
	if err != nil || job.Preset == nil || len(job.Preset.Roles) == 0 {
		return nil
	}
	book, err := openPresetReport(dir)
	if err != nil {
		r.host.warnf("turn %s could not open peer reporting", leased.ID)
	}
	return func(finished bool) {
		peers := readPresetReport(dir, book, job.Preset.Roles, finished)
		if book != nil {
			_ = book.file.Close()
			if book.current(dir) {
				_ = dir.parent.Remove(peerPointer)
			}
			_ = book.marker.Close()
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionACPCleanupTimeout)
		defer cancel()
		for _, role := range job.Preset.Roles {
			peer := peers[role.Name]
			payload, _ := json.Marshal(map[string]any{"role": role.Name, "mode": role.Mode, "status": peer.status, "failed_attempts": peer.failures})
			_, err := r.store.AppendEvent(writeCtx, session.AppendEventRequest{SessionID: bound.ID, TurnID: leased.ID, Type: session.EventPresetPeer, Version: 1, Payload: payload})
			if err != nil {
				r.host.warnf("turn %s could not record a peer outcome", leased.ID)
				return
			}
		}
	}
}
