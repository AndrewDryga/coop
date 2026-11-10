package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m.Run)) }

func scanFixture(img image) map[string]any {
	return map[string]any{
		"SchemaVersion": 2, "Metadata": map[string]any{"ImageID": img.ID},
		"Results": []any{
			map[string]any{"Class": "os-pkgs", "Type": "debian", "Packages": []any{map[string]string{"Name": "libbpf1"}, map[string]string{"Name": "libelf1"}, map[string]string{"Name": "zlib1g"}, map[string]string{"Name": "libc6"}}},
			map[string]any{"Class": "lang-pkgs", "Type": "gobinary", "Target": ownedBinary(img.Name), "Packages": []any{map[string]string{"Name": "stdlib"}}},
		},
	}
}

func TestScanRejectsIncompleteOrVulnerableReports(t *testing.T) {
	img := image{"filesystem", "sha256:" + strings.Repeat("a", 64)}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		want   string
	}{
		{"complete", func(map[string]any) {}, ""},
		{"wrong image", func(r map[string]any) { r["Metadata"] = map[string]string{"ImageID": "another"} }, "identify"},
		{"missing OS", func(r map[string]any) { r["Results"] = r["Results"].([]any)[1:] }, "OS package"},
		{"unrelated Go", func(r map[string]any) { r["Results"].([]any)[1].(map[string]any)["Target"] = "usr/local/bin/asdf" }, "Go binary"},
		{"missing Go", func(r map[string]any) { r["Results"] = r["Results"].([]any)[:1] }, "Go binary"},
		{"missing library", func(r map[string]any) {
			r["Results"].([]any)[0].(map[string]any)["Packages"] = []any{map[string]string{"Name": "libc6"}}
		}, "libbpf1"},
		{"vulnerable", func(r map[string]any) {
			r["Results"].([]any)[0].(map[string]any)["Vulnerabilities"] = []any{map[string]string{"VulnerabilityID": "CVE-fixture", "Severity": "HIGH", "FixedVersion": "1.2.3"}}
		}, "CVE-fixture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := scanFixture(img)
			tc.change(report)
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			_, err = verifyReport(data, img)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("report error: %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := verifyReport([]byte("broken"), img); err == nil {
		t.Fatal("invalid JSON passed")
	}
}

func TestClientScanRequiresInstalledPinnedPackages(t *testing.T) {
	img := image{"clients", "sha256:" + strings.Repeat("a", 64)}
	report := scanFixture(img)
	expected, err := bundledNPM()
	if err != nil {
		t.Fatal(err)
	}
	var packages []map[string]string
	for name, version := range expected {
		packages = append(packages, map[string]string{"Name": name, "Version": version, "FilePath": "opt/coop/clients/node_modules/" + name + "/package.json"})
	}
	report["Results"] = append(report["Results"].([]any), map[string]any{"Type": "node-pkg", "Packages": packages})
	check := func(wantError bool) {
		t.Helper()
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		_, err = verifyReport(data, img)
		if (err != nil) != wantError {
			t.Fatalf("error=%v, want failure %t", err, wantError)
		}
	}
	check(false)
	original := packages[0]["Version"]
	packages[0]["Version"] = "0.0.0"
	check(true)
	packages[0]["Version"] = original
	packages[0]["FilePath"] = "usr/local/lib/node_modules/package.json"
	check(true)
}

func TestScannerCommandAndFailurePropagation(t *testing.T) {
	img := image{"filesystem", "sha256:" + strings.Repeat("b", 64)}
	dir := t.TempDir()
	fixture, _ := json.Marshal(scanFixture(img))
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), fixture, 0644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
[ "${TRIVY_IGNORE_UNFIXED-unset}" = unset ] || exit 41
printf '%s\n' "$@" > "$FIXTURE_ARGS"
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = --output ]; then shift; output=$1; fi
  shift
done
[ -n "$output" ] || exit 42
if [ "$FIXTURE_EXIT" != 2 ]; then cp "$FIXTURE_REPORT" "$output"; fi
exit "$FIXTURE_EXIT"
`
	if err := os.WriteFile(filepath.Join(dir, "trivy"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TRIVY_IGNORE_UNFIXED", "true")
	t.Setenv("FIXTURE_ARGS", filepath.Join(dir, "args"))
	t.Setenv("FIXTURE_REPORT", filepath.Join(dir, "fixture.json"))
	for _, code := range []string{"0", "1", "2"} {
		t.Setenv("FIXTURE_EXIT", code)
		_, err := scan(context.Background(), "unix:///fixture.sock", img, filepath.Join(dir, "report-"+code+".json"))
		if code == "0" && err != nil || code != "0" && err == nil {
			t.Fatalf("scanner exit %s: %v", code, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--image-src\ndocker", "--docker-host\nunix:///fixture.sock", "--scanners\nvuln", "--pkg-types\nos,library", "--severity\nHIGH,CRITICAL", "--exit-code\n0", "--config\n/dev/null", "--ignorefile\n/dev/null", img.ID} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing scanner boundary %q", want)
		}
	}
}

func TestScanAllContinuesAfterBuildAndScanFailures(t *testing.T) {
	var attempted []string
	builders := []builder{
		{"filesystem", func(context.Context, *os.File) ([]image, error) {
			attempted = append(attempted, "build filesystem")
			return nil, errors.New("build fixture failure")
		}},
		{"base", func(context.Context, *os.File) ([]image, error) {
			attempted = append(attempted, "build base")
			return []image{{"base", "base-id"}}, nil
		}},
		{"clients-network", func(context.Context, *os.File) ([]image, error) {
			attempted = append(attempted, "build clients-network")
			return []image{{"clients", "clients-id"}, {"network", "network-id"}}, nil
		}},
	}
	dir := t.TempDir()
	err := scanAll(context.Background(), dir, builders, func(_ context.Context, img image, _ string) (findingCounts, error) {
		attempted = append(attempted, "scan "+img.Name)
		if img.Name == "base" {
			return findingCounts{Fixable: 1}, errors.New("scan fixture failure")
		}
		return findingCounts{Unfixed: 2}, nil
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "build fixture failure") || !strings.Contains(err.Error(), "scan fixture failure") {
		t.Fatalf("lost failures: %v", err)
	}
	if got := strings.Join(attempted, ","); got != "build filesystem,build base,scan base,build clients-network,scan clients,scan network" {
		t.Fatalf("incomplete attempts: %s", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var results []result
	if err := json.Unmarshal(data, &results); err != nil || len(results) != 4 || results[0].Error == "" || results[1].Error == "" || results[2].Error != "" || results[3].Error != "" || results[1].Fixable != 1 || results[2].Unfixed != 2 {
		t.Fatalf("inaccurate evidence: %s (%v)", data, err)
	}
}

func TestFindingsDoNotMaskMissingInventory(t *testing.T) {
	img := image{"filesystem", "sha256:" + strings.Repeat("c", 64)}
	report := scanFixture(img)
	report["Results"] = report["Results"].([]any)[:1]
	report["Results"].([]any)[0].(map[string]any)["Vulnerabilities"] = []any{map[string]string{"VulnerabilityID": "CVE-fixture", "Severity": "HIGH", "FixedVersion": "1.2.3"}}
	data, _ := json.Marshal(report)
	if _, err := verifyReport(data, img); err == nil || !strings.Contains(err.Error(), "Go binary inventory") || !strings.Contains(err.Error(), "CVE-fixture") {
		t.Fatalf("finding masked incomplete inventory: %v", err)
	}
}

func TestPolicyBlocksOnlyFixableSevereFindings(t *testing.T) {
	img := image{"filesystem", "sha256:" + strings.Repeat("d", 64)}
	for _, tc := range []struct {
		name, severity, fixedVersion string
		want                         findingCounts
		blocked                      bool
	}{
		{"fixed high", "HIGH", "1.2.3", findingCounts{Fixable: 1}, true},
		{"fixed critical", "CRITICAL", "1.2.3, 2.3.4", findingCounts{Fixable: 1}, true},
		{"unfixed high", "HIGH", "", findingCounts{Unfixed: 1}, false},
		{"unfixed critical", "CRITICAL", " ", findingCounts{Unfixed: 1}, false},
		{"fixed medium", "MEDIUM", "1.2.3", findingCounts{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := scanFixture(img)
			report["Results"].([]any)[0].(map[string]any)["Vulnerabilities"] = []any{map[string]string{
				"VulnerabilityID": "CVE-fixture", "Severity": tc.severity, "FixedVersion": tc.fixedVersion,
			}}
			data, _ := json.Marshal(report)
			counts, err := verifyReport(data, img)
			if counts != tc.want || (err != nil) != tc.blocked {
				t.Fatalf("counts=%+v error=%v; want %+v blocked=%t", counts, err, tc.want, tc.blocked)
			}
		})
	}
}

func TestPolicyReportsUnfixedFindingsOnPassingScans(t *testing.T) {
	builders := []builder{{"all", func(context.Context, *os.File) ([]image, error) {
		return []image{{"filesystem", "fs"}, {"base", "base"}, {"clients", "clients"}, {"network", "network"}}, nil
	}}}
	var output bytes.Buffer
	dir := t.TempDir()
	err := scanAll(context.Background(), dir, builders, func(context.Context, image, string) (findingCounts, error) {
		return findingCounts{Unfixed: 3}, nil
	}, &output)
	if err != nil || strings.Count(output.String(), "0 fixable HIGH/CRITICAL findings; 3 without a published fix") != 4 {
		t.Fatalf("unfixed findings lost: error=%v output=%s", err, &output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var results []result
	if err := json.Unmarshal(data, &results); err != nil || len(results) != 4 {
		t.Fatalf("summary: %s (%v)", data, err)
	}
	for _, r := range results {
		if r.Unfixed != 3 || r.Fixable != 0 || r.Error != "" {
			t.Fatalf("inaccurate passing summary: %+v", r)
		}
	}
}
