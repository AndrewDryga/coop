package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	cruntime "github.com/AndrewDryga/coop/internal/runtime"
)

type layout struct {
	name, base, target string
	ref                cruntime.DockerRef
	controlRef         cruntime.DockerRef
	control            map[string]readResult
}

type readResult struct {
	Result, Body string
	Errno        int
}

var readPaths = []string{".env", ".ENV", ".Env", ".eNv", ".enV", ".EnV", ".ENv", "env-link", "env-hard", "replacement-hard", "Café.pem", "CAFÉ.PEM", "Cafe\u0301.pem", "Café", "key.pem", "\u212aEY.PEM", "private/key", "PRIVATE/key", ".coopignore", ".COOPIGNORE", "policy-link", "policy-hard", "public.txt"}

func reads(data []byte, mode string) (map[string]readResult, error) {
	var result map[string]readResult
	expected := append([]string{}, readPaths...)
	switch mode {
	case "read":
	case "replacement":
		expected = append(expected, ".eNV")
	case "unicode":
		expected = append(expected, "café.pem", "cAFÉ.PEM", "caFe\u0301.PeM", "\u212aey.PeM", "cAFÉ", "caFe\u0301")
	default:
		return nil, errors.New("unknown read mode")
	}
	if json.Unmarshal(data, &result) != nil || len(result) != len(expected) {
		return nil, errors.New("missing read observations")
	}
	for _, key := range expected {
		value, ok := result[key]
		if !ok || !validRead(value) {
			return nil, fmt.Errorf("missing or invalid read observation: %s", key)
		}
	}
	return result, nil
}

func validRead(value readResult) bool {
	return value.Result == "read" && value.Errno == 0 || value.Result == "error" && value.Body == "" && (value.Errno == 1 || value.Errno == 2 || value.Errno == 13)
}

func custody(data []byte) error {
	var value struct {
		Cgroup, Loader int
		Status         string
	}
	if json.Unmarshal(data, &value) != nil || value.Cgroup != 30 && value.Cgroup != 13 && value.Cgroup != 1 || value.Loader != 30 && value.Loader != 13 && value.Loader != 1 {
		return errors.New("workload control paths writable or unobserved")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(value.Status, "\n") {
		key, body, ok := strings.Cut(line, ":")
		if ok {
			fields[key] = strings.TrimSpace(body)
		}
	}
	if fields["CapEff"] != "0000000000000000" || fields["CapPrm"] != "0000000000000000" || fields["CapBnd"] != "0000000000000000" || fields["NoNewPrivs"] != "1" {
		return errors.New("workload privilege controls failed")
	}
	return nil
}

// Count only demonstrated exposure, and retain all raw controls. An absent
// spelling is not evidence that an enforcing hook ran.
func exposure(data map[string]readResult) []string {
	var paths []string
	for path, value := range data {
		if value.Result == "read" && strings.HasPrefix(value.Body, "INERT_") {
			paths = append(paths, path)
		}
	}
	return paths
}

func (r *runner) observeReads(ctx context.Context, l layout, stage, mode string) error {
	data, err := r.probe(ctx, l.ref, l.target, stage, mode)
	if err != nil {
		return err
	}
	values, err := reads(data, mode)
	if err != nil {
		return err
	}
	if values["public.txt"].Result != "read" || !strings.HasPrefix(values["public.txt"].Body, "PUBLIC") {
		return errors.New("readable public control failed")
	}
	for _, path := range exposure(values) {
		r.report.Limitations = append(r.report.Limitations, l.name+" "+stage+": exposed "+path)
	}
	return nil
}

func (r *runner) experiments(ctx context.Context, f *fixture, bpf bool) error {
	layouts := []layout{{name: "agent", base: "workspace", target: "/workspace"}, {name: "service", base: "workspace/nested", target: "/data"}}
	var groups [2]uint64
	for i := range layouts {
		l := &layouts[i]
		ref, err := r.workload(ctx, f, l.name+"-control", l.base, l.target, false)
		if err != nil {
			return err
		}
		l.controlRef = ref
		identity, err := r.probe(ctx, ref, l.target, "control-identity", "identity")
		if err != nil {
			return err
		}
		if _, err = cgroupIdentity(identity, f.name); err != nil {
			return err
		}
		if err = f.verifyBind(l.base); err != nil {
			return err
		}
		data, err := r.probe(ctx, ref, l.target, "unmasked-control", "read")
		if err != nil {
			return err
		}
		l.control, err = reads(data, "read")
		if err != nil {
			return err
		}
		for _, path := range []string{".env", "Café.pem", "key.pem", "env-hard", "env-link", "private/key"} {
			if l.control[path].Body != "INERT_SECRET" {
				return fmt.Errorf("unmasked fixture control failed: %s", path)
			}
		}
		if l.control[".coopignore"].Body != "fixture policy\n" || l.control["public.txt"].Body != "PUBLIC" {
			return errors.New("unmasked policy/public control failed")
		}
		l.ref, err = r.workload(ctx, f, l.name, l.base, l.target, !bpf)
		if err != nil {
			return err
		}
		data, err = r.probe(ctx, l.ref, l.target, "identity", "identity")
		if err != nil {
			return err
		}
		groups[i], err = cgroupIdentity(data, f.name)
		if err != nil {
			return err
		}
	}
	if groups[0] == groups[1] {
		return errors.New("agent and service did not receive distinct cgroups")
	}
	if bpf {
		if err := r.attach(ctx, f, groups); err != nil {
			return err
		}
	}
	for _, l := range layouts {
		if err := r.observeReads(ctx, l, "original", "read"); err != nil {
			return err
		}
		control, err := r.probe(ctx, l.ref, l.target, "control-custody", "custody")
		if err != nil {
			return err
		}
		if err := custody(control); err != nil {
			return err
		}
		if err := f.write(l.base+"/public.txt", "PUBLIC_HOST_EDIT"); err != nil {
			return err
		}
		data, err := r.probe(ctx, l.ref, l.target, "public-workflow", "public")
		if err != nil {
			return err
		}
		var public struct{ Created, Host readResult }
		if json.Unmarshal(data, &public) != nil || public.Created.Body != "GUEST_PUBLIC_EDIT" || public.Host.Body != "PUBLIC_HOST_EDIT" {
			return errors.New("public workflow failed")
		}
		host, err := f.root.ReadFile(l.base + "/renamed-public")
		if err != nil || string(host) != "GUEST_PUBLIC_EDIT" {
			return errors.Join(errors.New("guest write did not reach host"), err)
		}
		// Real host replacement, followed by an alias before any guest lookup.
		if err = f.write(l.base+"/editor-save", "INERT_REPLACEMENT"); err != nil {
			return err
		}
		if err = f.root.Rename(l.base+"/editor-save", l.base+"/.env"); err != nil {
			return err
		}
		if err = f.root.Link(l.base+"/.env", l.base+"/replacement-hard"); err != nil {
			return err
		}
		if err = r.observeReads(ctx, l, "host-atomic-replacement", "replacement"); err != nil {
			return err
		}
		for _, name := range []string{"Café.pem", "Café", "key.pem"} {
			if err = f.write(l.base+"/editor-save", "INERT_UNICODE_REPLACEMENT"); err != nil {
				return err
			}
			if err = f.root.Rename(l.base+"/editor-save", l.base+"/"+name); err != nil {
				return err
			}
		}
		if err = r.observeReads(ctx, l, "unicode-replacement", "unicode"); err != nil {
			return err
		}
		control, err = r.probe(ctx, l.controlRef, l.target, "unmasked-unicode-replacement", "unicode")
		if err != nil {
			return err
		}
		values, err := reads(control, "unicode")
		if err != nil || values["Café.pem"].Body != "INERT_UNICODE_REPLACEMENT" || values["replacement-hard"].Body != "INERT_REPLACEMENT" {
			return errors.Join(errors.New("host replacement control unavailable"), err)
		}
		if err = r.policy(ctx, f, l); err != nil {
			return err
		}
		if bpf {
			if err = r.guestAliases(ctx, f, l); err != nil {
				return err
			}
			if err = r.linkMetadata(ctx, f, l); err != nil {
				return err
			}
			if err = r.heldDescriptor(ctx, f, l); err != nil {
				return err
			}
		}
	}
	// Replacing a whole ancestor changes the parent identity used by the candidate.
	// The agent's existing workspace mount still reaches the new nested directory.
	if err := f.root.Rename("workspace/nested", "workspace/old-nested"); err != nil {
		return err
	}
	if err := f.root.Mkdir("workspace/nested", 0777); err != nil {
		return err
	}
	if err := f.write("workspace/nested/.env", "INERT_ANCESTOR_REPLACEMENT"); err != nil {
		return err
	}
	data, err := r.probe(ctx, layouts[0].ref, "/workspace", "ancestor-replacement", "ancestor")
	if err != nil {
		return errors.Join(errors.New("ancestor probe incomplete"), err)
	}
	var ancestor readResult
	if json.Unmarshal(data, &ancestor) != nil || !validRead(ancestor) {
		return errors.New("invalid ancestor evidence")
	}
	if ancestor.Body == "INERT_ANCESTOR_REPLACEMENT" {
		r.report.Limitations = append(r.report.Limitations, "agent ancestor replacement: exposed nested/.env")
	}
	// An unrelated cgroup must remain able to read the same inert files.
	for _, ref := range r.refs {
		if strings.HasSuffix(ref.Name, "-agent-control") {
			data, err := r.probe(ctx, ref, "/workspace", "unrelated-cgroup", "read")
			if err != nil {
				return err
			}
			values, err := reads(data, "read")
			if err != nil || values[".env"].Body != "INERT_REPLACEMENT" {
				return errors.Join(errors.New("unrelated cgroup was affected"), err)
			}
		}
	}
	if err := r.docker.Verify(ctx); err != nil {
		return err
	}
	if err := f.verify(); err != nil {
		return err
	}
	if bpf {
		if err := r.openTelemetry(ctx); err != nil {
			return err
		}
	}
	// Fixed rules are not Coop's full policy. Never turn evidence into an admission grant.
	r.report.Qualified = false
	return nil
}

func (r *runner) policy(ctx context.Context, f *fixture, l layout) error {
	for _, spelling := range []string{".coopignore", ".COOPIGNORE", "policy-link", "policy-hard"} {
		for index, operation := range []string{"write", "truncate", "unlink", "rename", "replace", "link"} {
			alias := spelling
			if spelling == "policy-link" || spelling == "policy-hard" {
				alias = fmt.Sprintf("%s-%d", spelling, index)
			}
			if spelling == ".COOPIGNORE" {
				name := []byte(".coopignore")
				for bit := 0; bit < 10; bit++ {
					if (index+1)&(1<<bit) != 0 {
						name[bit+1] -= 32
					}
				}
				alias = string(name)
			}
			for _, name := range []string{".coopignore", "policy-hard", "policy-link", "moved-policy", "policy-save", "new-policy-link"} {
				if err := f.root.Remove(l.base + "/" + name); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			if err := f.write(l.base+"/.coopignore", "fixture policy\n"); err != nil {
				return err
			}
			if err := f.root.Chmod(l.base+"/.coopignore", 0666); err != nil {
				return err
			}
			if err := f.root.Link(l.base+"/.coopignore", l.base+"/policy-hard"); err != nil {
				return err
			}
			if err := f.root.Symlink(".coopignore", l.base+"/policy-link"); err != nil {
				return err
			}
			if spelling == "policy-link" {
				if err := f.root.Symlink(".coopignore", l.base+"/"+alias); err != nil {
					return err
				}
			}
			if spelling == "policy-hard" {
				if err := f.root.Link(l.base+"/.coopignore", l.base+"/"+alias); err != nil {
					return err
				}
			}
			before, err := f.root.Stat(l.base + "/.coopignore")
			if err != nil {
				return err
			}
			data, err := r.probe(ctx, l.ref, l.target, "policy-"+operation+"-"+alias, "mutate", operation, alias)
			if err != nil {
				return err
			}
			var value struct{ Before, Outcome readResult }
			if json.Unmarshal(data, &value) != nil {
				return errors.New("invalid policy evidence")
			}
			after, statErr := f.root.Stat(l.base + "/.coopignore")
			body, readErr := f.root.ReadFile(l.base + "/.coopignore")
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) || readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return errors.Join(statErr, readErr)
			}
			changed := statErr != nil || !os.SameFile(before, after) || string(body) != "fixture policy\n"
			r.report.Observations = append(r.report.Observations, observation{Layout: l.name, Stage: "host-policy-" + operation + "-" + spelling, Evidence: raw(map[string]any{"exists": statErr == nil, "same_inode": statErr == nil && os.SameFile(before, after), "body": string(body), "changed": changed})})
			if changed {
				r.report.Limitations = append(r.report.Limitations, l.name+" host policy changed: "+operation+" "+spelling)
			}
			if value.Before.Result == "error" && value.Before.Errno == 2 {
				// Case-sensitive shares do not resolve this alias. Record it without
				// claiming a successful enforcement test.
				r.report.Limitations = append(r.report.Limitations, l.name+" policy alias absent: "+spelling)
				continue
			}
			if value.Before.Body != "fixture policy\n" {
				if !validRead(value.Before) {
					return errors.New("policy readable control unavailable")
				}
				r.report.Limitations = append(r.report.Limitations, l.name+" policy alias has stale or inaccessible contents: "+operation+" "+alias)
			}
			if value.Outcome.Result != "mutated" && (value.Outcome.Result != "error" || value.Outcome.Errno != 1 && value.Outcome.Errno != 13 && value.Outcome.Errno != 16 && value.Outcome.Errno != 30) {
				return fmt.Errorf("unexpected policy mutation result: %s", data)
			}
		}
	}
	return nil
}
