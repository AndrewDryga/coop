package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestFilteredNativeBindDescriptorsKeepExistingCustodyChecks(t *testing.T) {
	f, _ := filteredFixture(t)
	file, err := writeTempFile(f.runfiles, "public-certificate")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, descriptor string
		files            []string
		allowed          bool
	}{
		{"public generated CA", networkMount("bind", file, "/public/ca.pem", true), []string{file}, true},
		{"unregistered generated file", networkMount("bind", file, "/public/ca.pem", true), nil, false},
		{"authority root", networkMount("bind", f.store.Path(), "/authority", false), nil, false},
		{"root source", networkMount("bind", "/", "/host", true), nil, false},
		{"relative source", "type=bind,source=relative,target=/x", nil, false},
		{"named volume", "type=volume,source=public,target=/x", nil, false},
		{"tmpfs", "type=tmpfs,target=/x", nil, false},
		{"missing type", "source=/tmp,target=/x", nil, false},
		{"unknown field", "type=bind,source=/tmp,target=/x,bind-propagation=shared", nil, false},
		{"duplicate source alias", "type=bind,source=/tmp,src=/var,target=/x", nil, false},
		{"duplicate readonly alias", "type=bind,source=/tmp,target=/x,readonly,ro=false", nil, false},
		{"junk readonly", "type=bind,source=/tmp,target=/x,readonly=maybe", nil, false},
		{"readonly delimiter in target", "type=bind,source=/tmp,target=/x:ro", nil, false},
		{"delimiter in source", "type=bind,source=/tmp:alias,target=/x", nil, false},
		{"second descriptor", "type=bind,source=/tmp,target=/x\ntype=bind,source=/var,target=/y", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := []string{"--mount", test.descriptor}
			if err := f.validateMounts(options, test.files, nil); (err == nil) != test.allowed {
				t.Fatalf("allowed=%v, want%v: %v", err == nil, test.allowed, err)
			}
			if test.allowed {
				if options[0] != "--mount" {
					t.Fatal("bind descriptor became an auto-creating volume shorthand")
				}
				plan, err := networkMountPlan(options)
				if err != nil || len(plan) != 1 || plan["/public/ca.pem"].Source != file || plan["/public/ca.pem"].RW {
					t.Fatal("public mount topology changed", err)
				}
				if info := f.bindSources[file]; info == nil {
					t.Fatal("native descriptor did not retain generated inode binding")
				}
			}
		})
	}
}

func TestFilteredNativeGeneratedBindReplacementRefuses(t *testing.T) {
	f, _ := filteredFixture(t)
	file, err := writeTempFile(f.runfiles, "public-helper")
	if err != nil {
		t.Fatal(err)
	}
	options := []string{"--mount", networkMount("bind", file, "/public/helper", true)}
	if err := f.validateMounts(options, []string{file}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(file, file+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("replacement helper"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.checkBindings(); err == nil {
		t.Fatal("replacement native helper retained stale inode authority")
	}
}

func TestFilteredNativeHomeAssemblyUsesValidatedCSV(t *testing.T) {
	f, _ := filteredFixture(t)
	boxHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BoxHome: boxHome, ConfigDir: filepath.Join(boxHome, "agents"), HomeInBox: "/home/node"}
	spec := RunSpec{Repo: f.record.Project, Agent: "gemini", Homes: true}
	profile := filepath.Join(cfg.ConfigDir, "gemini", "native-homes", "default", `repo, "quoted"`, "home")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg = cfg.WithNativeHomes(map[string]string{"gemini": profile})
	f.unsafeRoots = filteredWritableRoots(cfg, spec)
	f.authorityConfig, f.authoritySpec = cfg, spec
	options := assembleOptions(cfg, false, spec, nil, "", "", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	plan, err := networkMountPlan(options)
	if err != nil || plan["/home/node/.gemini"].Source != profile || !plan["/home/node/.gemini"].RW {
		t.Fatal("actual native home emission is not an exact bind", err)
	}
	if err := f.validateMounts(options, nil, nil); err != nil {
		t.Fatal("actual emitted native home refused", err)
	}
	child := filepath.Join(profile, "agent-owned")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	unsafe := append(append([]string(nil), options...), "--mount", networkMount("bind", child, "/nested", true))
	if err := f.validateMounts(unsafe, nil, nil); err == nil {
		t.Fatal("native syntax bypassed writable-parent refusal")
	}
}

func TestFilteredNativeBindCanonicalizationPreservesCSVAndWritableState(t *testing.T) {
	for _, readonly := range []bool{false, true} {
		f, _ := filteredFixture(t)
		real := filepath.Join(t.TempDir(), `public, "quoted"`)
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		canonical, err := filepath.EvalSymlinks(real)
		if err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(real, alias); err != nil {
			t.Fatal(err)
		}
		descriptor := "type=bind,src=" + alias + ",dst=/native,readonly=false"
		if readonly {
			descriptor = strings.TrimSuffix(descriptor, "false") + "true"
		}
		options := []string{"--mount", descriptor}
		if err := f.validateMounts(options, nil, nil); err != nil {
			t.Fatal(err)
		}
		plan, err := networkMountPlan(options)
		if err != nil || options[0] != "--mount" || len(plan) != 1 || plan["/native"].Source != canonical || plan["/native"].RW == readonly {
			t.Fatal("canonical bind descriptor disagrees with final topology", err)
		}
	}
}
