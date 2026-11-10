// imagevuln builds and scans the release-owned images, without qualifying or
// changing a user's network setup. Make owns the scanner version and CI entry.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/fsimage"
	"github.com/AndrewDryga/coop/internal/runtime"
)

type image struct{ Name, ID string }
type findingCounts struct{ Fixable, Unfixed int }
type result struct {
	Name, ID, Report, Error string
	findingCounts
}
type builder struct {
	name  string
	build func(context.Context, *os.File) ([]image, error)
}

func main() {
	version := flag.String("trivy-version", "", "required scanner version (from Make)")
	out := flag.String("out", ".agent/image-vuln", "parent directory for a new report folder")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, *version, *out, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, version, out string, output io.Writer) error {
	if version == "" {
		return errors.New("use make image-vuln to select the pinned scanner")
	}
	data, err := exec.CommandContext(ctx, "trivy", "--version", "--format", "json").Output()
	var installed struct{ Version string }
	if err != nil {
		return fmt.Errorf("trivy unavailable: %w; run make install-trivy", err)
	}
	if json.Unmarshal(data, &installed) != nil || installed.Version != version {
		return fmt.Errorf("trivy must be %s; run make install-trivy", version)
	}
	if err := os.MkdirAll(out, 0755); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(out, "scan-")
	if err != nil {
		return err
	}
	fmt.Fprintln(output, "Image scan evidence:", dir)
	rt, err := runtime.DetectContext(ctx, "docker")
	if err != nil {
		return err
	}
	docker, err := runtime.BindDockerForSetup(ctx, rt, "", "")
	if err != nil {
		return err
	}
	defer docker.Close()
	builders := []builder{
		{"filesystem", func(ctx context.Context, log *os.File) ([]image, error) {
			id, err := fsimage.Build(ctx, docker, log)
			return []image{{"filesystem", id}}, err
		}},
		{"base", func(ctx context.Context, log *os.File) ([]image, error) {
			id, err := box.BuildBaseCandidate(ctx, docker, log, log)
			return []image{{"base", id}}, err
		}},
		{"clients-network", func(ctx context.Context, log *os.File) ([]image, error) {
			c, err := box.BuildNetworkCandidate(ctx, docker, log, log)
			return []image{{"clients", c.ClientImage}, {"network", c.GatewayImage}}, err
		}},
	}
	return scanAll(ctx, dir, builders, func(ctx context.Context, img image, report string) (findingCounts, error) {
		return scan(ctx, docker.Endpoint(), img, report)
	}, output)
}

func scanAll(ctx context.Context, dir string, builders []builder, scanImage func(context.Context, image, string) (findingCounts, error), output io.Writer) error {
	var results []result
	var failures []error
	for _, b := range builders {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		log, err := os.Create(filepath.Join(dir, b.name+"-build.log"))
		if err != nil {
			return err
		}
		fmt.Fprintln(output, "Building", b.name)
		buildCtx, stop := context.WithTimeout(ctx, 40*time.Minute)
		images, buildErr := b.build(buildCtx, log)
		stop()
		buildErr = errors.Join(buildErr, log.Close())
		if buildErr != nil {
			failures = append(failures, fmt.Errorf("%s build: %w", b.name, buildErr))
			results = append(results, result{Name: b.name, Error: buildErr.Error()})
			fmt.Fprintln(output, b.name, "build failed; see build log")
			continue
		}
		for _, img := range images {
			fmt.Fprintln(output, "Scanning", img.Name, img.ID)
			report := filepath.Join(dir, img.Name+".json")
			counts, err := scanImage(ctx, img, report)
			if err != nil {
				fmt.Fprintln(output, img.Name+": check failed; the following report counts may be incomplete")
			}
			fmt.Fprintf(output, "%s: %d fixable HIGH/CRITICAL findings; %d without a published fix (see report)\n", img.Name, counts.Fixable, counts.Unfixed)
			r := result{Name: img.Name, ID: img.ID, Report: report, findingCounts: counts}
			if err != nil {
				r.Error = err.Error()
				failures = append(failures, fmt.Errorf("%s scan: %w", img.Name, err))
			}
			results = append(results, r)
		}
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), append(data, '\n'), 0644); err != nil {
		return err
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	if len(results) != 4 {
		return errors.New("incomplete image scan")
	}
	fmt.Fprintln(output, "All four images passed the fixable HIGH/CRITICAL policy; findings without fixes remain in reports.")
	return nil
}

func scan(ctx context.Context, endpoint string, img image, report string) (findingCounts, error) {
	if !strings.HasPrefix(img.ID, "sha256:") || len(img.ID) != 71 {
		return findingCounts{}, errors.New("scan requires an immutable image ID")
	}
	log, err := os.Create(report + ".log")
	if err != nil {
		return findingCounts{}, err
	}
	defer log.Close()
	cmd := exec.CommandContext(ctx, "trivy", "image", "--config", os.DevNull, "--ignorefile", os.DevNull, "--image-src", "docker", "--docker-host", endpoint,
		"--scanners", "vuln", "--pkg-types", "os,library", "--severity", "HIGH,CRITICAL", "--exit-code", "0",
		"--format", "json", "--list-all-pkgs", "--no-progress", "--timeout", "15m", "--output", report, img.ID)
	// Ambient scanner settings must not suppress findings or skip database updates.
	for _, value := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(value), "TRIVY_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Stdout, cmd.Stderr = log, log
	scanErr := cmd.Run()
	data, readErr := os.ReadFile(report)
	if readErr != nil {
		return findingCounts{}, errors.Join(scanErr, readErr)
	}
	// Preserve all findings in JSON. Evaluate fixability here rather than hiding
	// unfixed advisories with the scanner's --ignore-unfixed option.
	counts, reportErr := verifyReport(data, img)
	return counts, errors.Join(scanErr, reportErr)
}

func verifyReport(data []byte, img image) (findingCounts, error) {
	var counts findingCounts
	var report struct {
		SchemaVersion int
		Metadata      struct{ ImageID string }
		Results       []struct {
			Class, Type, Target string
			Packages            []struct{ Name, Version, FilePath string }
			Vulnerabilities     []struct{ VulnerabilityID, Severity, FixedVersion string }
		}
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return counts, err
	}
	if report.SchemaVersion != 2 || report.Metadata.ImageID != img.ID {
		return counts, errors.New("report does not identify the scanned image")
	}
	osPackages := make(map[string]bool)
	var goBinary bool
	var findings []error
	firstFinding := ""
	installedNPM := make(map[string]string)
	for _, r := range report.Results {
		for _, p := range r.Packages {
			if r.Class == "os-pkgs" {
				osPackages[p.Name] = true
			}
		}
		if r.Type == "node-pkg" {
			for _, p := range r.Packages {
				if strings.TrimPrefix(p.FilePath, "/") == "opt/coop/clients/node_modules/"+p.Name+"/package.json" {
					installedNPM[p.Name] = p.Version
				}
			}
		}
		if r.Type == "gobinary" && len(r.Packages) > 0 && strings.TrimPrefix(r.Target, "/") == ownedBinary(img.Name) {
			goBinary = true
		}
		for _, v := range r.Vulnerabilities {
			if v.Severity == "HIGH" || v.Severity == "CRITICAL" {
				if strings.TrimSpace(v.FixedVersion) == "" {
					counts.Unfixed++
					continue
				}
				counts.Fixable++
				if firstFinding == "" {
					firstFinding = v.VulnerabilityID
				}
			}
		}
	}
	if counts.Fixable > 0 {
		findings = append(findings, fmt.Errorf("%d fixable HIGH/CRITICAL findings (including %s); see report", counts.Fixable, firstFinding))
	}
	if len(osPackages) == 0 {
		findings = append(findings, errors.New("scan has no OS package inventory"))
	}
	if !goBinary {
		findings = append(findings, fmt.Errorf("scan has no Go binary inventory for %s", ownedBinary(img.Name)))
	}
	if img.Name == "base" || img.Name == "clients" {
		expected, err := bundledNPM()
		if err != nil {
			return counts, err
		}
		for name, version := range expected {
			if installedNPM[name] != version {
				findings = append(findings, fmt.Errorf("scan omitted bundled npm package %s@%s", name, version))
			}
		}
	}
	if img.Name == "filesystem" {
		for _, name := range []string{"libbpf1", "libelf1", "zlib1g", "libc6"} {
			if !osPackages[name] {
				findings = append(findings, fmt.Errorf("scan omitted runtime library %s", name))
			}
		}
	}
	return counts, errors.Join(findings...)
}

// Both architectures share this manifest. Image scanning checks installed packages;
// npm-audit separately checks the whole lock, including other platform variants.
func bundledNPM() (map[string]string, error) {
	closure, err := agents.LockedClientClosure(agents.ClientPlatform{OS: "linux", Architecture: "amd64", Libc: "glibc"})
	if err != nil {
		return nil, err
	}
	var manifest struct{ Dependencies map[string]string }
	if err := json.Unmarshal(closure.Files["package.json"], &manifest); err != nil {
		return nil, err
	}
	if len(manifest.Dependencies) == 0 {
		return nil, errors.New("empty bundled npm manifest")
	}
	return manifest.Dependencies, nil
}

// Require the shipped Coop executable, not merely another Go binary such as asdf.
func ownedBinary(name string) string {
	switch name {
	case "base", "clients":
		return "usr/local/libexec/coop-supervise"
	case "network":
		return "usr/local/bin/coop-net"
	case "filesystem":
		return "usr/local/bin/coop-fs"
	default:
		return ""
	}
}
