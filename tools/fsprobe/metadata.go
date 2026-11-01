package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (r *runner) heldDescriptor(ctx context.Context, f *fixture, l layout) error {
	if err := f.write(l.base+"/held-public", "PUBLIC_HOST_EDIT"); err != nil {
		return err
	}
	step, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	type outcome struct {
		data []byte
		err  error
	}
	finished := make(chan outcome, 1)
	go func() {
		data, err := r.docker.ExecRead(step, l.ref, 4096, "/usr/bin/python3", "/probe/probe.py", l.target, "hold")
		finished <- outcome{data, err}
	}()
	// Join before returning, including failed setup. Report writes stay serialized.
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-finished
		}
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
waiting:
	for {
		select {
		case result := <-finished:
			joined = true
			return errors.Join(fmt.Errorf("held descriptor exited before readiness: %s", result.data), result.err)
		case <-step.Done():
			return step.Err()
		case <-deadline.C:
			return errors.New("held descriptor readiness not observed")
		case <-tick.C:
			body, err := f.root.ReadFile(l.base + "/held-ready")
			if err == nil && string(body) == "READY" {
				break waiting
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if err := f.root.Remove(l.base + "/.env"); err != nil {
		return err
	}
	if err := f.root.Link(l.base+"/held-public", l.base+"/.env"); err != nil {
		return err
	}
	if err := f.write(l.base+"/held-public", "INERT_REPLACEMENT"); err != nil {
		return err
	}
	if err := f.write(l.base+"/held-release", "RELEASE"); err != nil {
		return err
	}
	result := <-finished
	joined = true
	var value struct {
		Descriptor, Mapping string
		Fresh               readResult `json:"fresh_open"`
	}
	if result.err != nil || json.Unmarshal(result.data, &value) != nil || !validRead(value.Fresh) || value.Descriptor == "" || value.Mapping == "" {
		return errors.Join(errors.New("held descriptor observation incomplete"), result.err)
	}
	r.report.Observations = append(r.report.Observations, observation{Layout: l.name, Stage: "held-descriptor", Evidence: result.data})
	if strings.HasPrefix(value.Descriptor, "INERT_") || strings.HasPrefix(value.Mapping, "INERT_") {
		r.report.Limitations = append(r.report.Limitations, l.name+" existing descriptor/mapping retains access after host adds protected alias")
	}
	return nil
}

func (r *runner) guestAliases(ctx context.Context, f *fixture, l layout) error {
	for _, operation := range []string{"link", "rename", "replace"} {
		if err := f.write(l.base+"/editor-save", "INERT_REPLACEMENT"); err != nil {
			return err
		}
		if err := f.root.Rename(l.base+"/editor-save", l.base+"/.env"); err != nil {
			return err
		}
		data, err := r.probe(ctx, l.ref, l.target, "guest-secret-"+operation, "guest-alias", operation)
		var value struct{ Outcome, Alias readResult }
		if err != nil || json.Unmarshal(data, &value) != nil || !validRead(value.Alias) ||
			value.Outcome.Result != "mutated" && (value.Outcome.Result != "error" || value.Outcome.Errno != 1 && value.Outcome.Errno != 13) {
			return errors.Join(errors.New("guest alias evidence incomplete"), err)
		}
		body, err := f.root.ReadFile(l.base + "/.env")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if value.Outcome.Result == "mutated" || string(body) != "INERT_REPLACEMENT" {
			r.report.Limitations = append(r.report.Limitations, l.name+" guest secret mutation allowed: "+operation)
		}
		if err := f.root.Remove(l.base + "/guest-alias"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Host mutations run between guest opens; no guest stat can refresh attributes
// before the LSM records its decision-time metadata.
func (r *runner) linkMetadata(ctx context.Context, f *fixture, l layout) error {
	open := func(name, stage string) error {
		data, err := r.probe(ctx, l.ref, l.target, stage, "open-first", name)
		var result readResult
		if err != nil || json.Unmarshal(data, &result) != nil || !validRead(result) {
			return errors.Join(errors.New("link metadata read incomplete"), err)
		}
		if name == "public-hard" && result.Result == "error" && result.Errno != 2 {
			r.report.Limitations = append(r.report.Limitations, l.name+" legitimate public hardlink blocked by experimental multi-link restriction")
		}
		if stage == "link-fresh-first" || stage == "link-warm-after-add" {
			if strings.HasPrefix(result.Body, "INERT_") {
				r.report.Limitations = append(r.report.Limitations, l.name+" "+stage+": protected host alias exposed")
			}
		}
		info, err := f.root.Stat(l.base + "/" + name)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		r.report.Observations = append(r.report.Observations, observation{Layout: l.name, Stage: stage + "-host", Evidence: raw(map[string]any{"path": name, "inode": st.Ino, "links": st.Nlink})})
		return nil
	}
	if err := f.write(l.base+"/editor-save", "INERT_REPLACEMENT"); err != nil {
		return err
	}
	if err := f.root.Rename(l.base+"/editor-save", l.base+"/.env"); err != nil {
		return err
	}
	if err := f.root.Link(l.base+"/.env", l.base+"/fresh-host-alias"); err != nil {
		return err
	}
	if err := open("fresh-host-alias", "link-fresh-first"); err != nil {
		return err
	}
	if err := f.write(l.base+"/warm-add", "INERT_REPLACEMENT"); err != nil {
		return err
	}
	if err := open("warm-add", "link-warm-before-add"); err != nil {
		return err
	}
	if err := f.root.Remove(l.base + "/.env"); err != nil {
		return err
	}
	if err := f.root.Link(l.base+"/warm-add", l.base+"/.env"); err != nil {
		return err
	}
	if err := open("warm-add", "link-warm-after-add"); err != nil {
		return err
	}
	if err := f.write(l.base+"/warm-remove", "PUBLIC"); err != nil {
		return err
	}
	if err := f.root.Link(l.base+"/warm-remove", l.base+"/public-hard"); err != nil {
		return err
	}
	if err := open("warm-remove", "link-warm-before-remove"); err != nil {
		return err
	}
	if err := f.root.Remove(l.base + "/public-hard"); err != nil {
		return err
	}
	if err := open("warm-remove", "link-warm-after-remove"); err != nil {
		return err
	}
	if err := f.write(l.base+"/public-linked", "PUBLIC"); err != nil {
		return err
	}
	if err := f.root.Link(l.base+"/public-linked", l.base+"/public-hard"); err != nil {
		return err
	}
	return open("public-hard", "link-public-control")
}

type openRecord struct {
	PathHex    string `json:"path_hex,omitempty"`
	Path       string `json:"path"`
	FirstInode uint64 `json:"first_ino"`
	LastInode  uint64 `json:"last_ino"`
	FirstLinks uint32 `json:"first_links"`
	LastLinks  uint32 `json:"last_links"`
	Opens      uint64 `json:"opens"`
	Device     uint32 `json:"dev"`
}

func decodeTelemetry(data []byte) (map[string]openRecord, error) {
	var entries []openRecord
	if json.Unmarshal(data, &entries) != nil || len(entries) == 0 || len(entries) >= 512 {
		return nil, errors.New("missing or saturated open telemetry")
	}
	result := map[string]openRecord{}
	for _, entry := range entries {
		path, err := hex.DecodeString(entry.PathHex)
		if err != nil || len(path) == 0 || len(path) >= 256 || path[0] != '/' || strings.ContainsRune(string(path), 0) || entry.FirstInode == 0 || entry.LastInode == 0 || entry.Opens == 0 {
			return nil, errors.New("invalid open telemetry")
		}
		entry.Path, entry.PathHex = string(path), ""
		if _, duplicate := result[entry.Path]; duplicate {
			return nil, errors.New("duplicate open telemetry path")
		}
		result[entry.Path] = entry
	}
	return result, nil
}

func (r *runner) openTelemetry(ctx context.Context) error {
	data, err := r.observer(ctx, "open-telemetry", "--dump-map", strconv.FormatUint(uint64(r.mapID), 10))
	if err != nil {
		return err
	}
	entries, err := decodeTelemetry(data)
	if err != nil {
		return err
	}
	for _, target := range []string{"/workspace", "/data"} {
		layout := "agent"
		if target == "/data" {
			layout = "service"
		}
		for _, name := range []string{"warm-add", "warm-remove", "fresh-host-alias", "public-hard"} {
			entry, ok := entries[target+"/"+name]
			if !ok || (name == "warm-add" || name == "warm-remove") && entry.Opens < 2 {
				return fmt.Errorf("missing repeated metadata observation: %s/%s", target, name)
			}
			var lastHost uint32
			for _, observation := range r.report.Observations {
				if observation.Layout != layout || !strings.HasPrefix(observation.Stage, "link-") || !strings.HasSuffix(observation.Stage, "-host") {
					continue
				}
				var host struct {
					Path  string
					Links uint32
				}
				if json.Unmarshal(observation.Evidence, &host) != nil {
					return errors.New("invalid host link metadata")
				}
				if host.Path == name {
					lastHost = host.Links
				}
			}
			if lastHost == 0 {
				return fmt.Errorf("missing host link observation: %s/%s", target, name)
			}
			if entry.LastLinks != lastHost {
				r.report.Limitations = append(r.report.Limitations, fmt.Sprintf("%s %s: open-time kernel links=%d, host links=%d", layout, name, entry.LastLinks, lastHost))
			}
		}
	}
	r.report.Observations = append(r.report.Observations, observation{Layout: "kernel", Stage: "file-open-metadata", Evidence: raw(entries)})
	return nil
}
