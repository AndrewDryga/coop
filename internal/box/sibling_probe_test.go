package box

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The shell always exits zero; curl's terminal status is framed independently of transport.
const siblingCurlScript = `printf 'COOP_PROBE_BEGIN\n'
curl -q --proxy '' --silent --max-time 5 -o /dev/null "$1"
status=$?
printf 'COOP_PROBE_EXIT %s\n' "$status"
exit 0`

func siblingCurlExit(data []byte, transport error) (int, error) {
	if transport != nil {
		return -1, transport
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) != 3 || lines[0] != "COOP_PROBE_BEGIN" || lines[2] != "" || !strings.HasPrefix(lines[1], "COOP_PROBE_EXIT ") {
		return -1, errors.New("incomplete sibling probe frames")
	}
	text := strings.TrimPrefix(lines[1], "COOP_PROBE_EXIT ")
	code, err := strconv.Atoi(text)
	if err != nil || strconv.Itoa(code) != text || (code != 0 && code != 7 && code != 28) {
		return -1, fmt.Errorf("unexpected sibling curl status %s", lines[1])
	}
	return code, nil
}

func TestSiblingProbeDistinguishesCurlFromTransportFailure(t *testing.T) {
	bin := t.TempDir()
	record := filepath.Join(bin, "argv")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + strconv.Quote(record) + "\nexit \"$COOP_TEST_CURL_STATUS\"\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{0, 7, 28} {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", siblingCurlScript, "probe", "http://fixture/")
		cmd.Env = []string{"PATH=" + bin, "COOP_TEST_CURL_STATUS=" + strconv.Itoa(code)}
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("framing shell failed: %v\n%s", err, data)
		}
		if string(data) != fmt.Sprintf("COOP_PROBE_BEGIN\nCOOP_PROBE_EXIT %d\n", code) {
			t.Fatalf("terminal curl%d frame: %q", code, data)
		}
		args, err := os.ReadFile(record)
		if err != nil || string(args) != "-q\n--proxy\n\n--silent\n--max-time\n5\n-o\n/dev/null\nhttp://fixture/\n" {
			t.Fatalf("curl request changed: %q, %v", args, err)
		}
		if got, err := siblingCurlExit(data, nil); err != nil || got != code {
			t.Fatalf("curl%d = %d, %v", code, got, err)
		}
		if _, err := siblingCurlExit(data, errors.New("transport failed after output")); err == nil {
			t.Fatal("transport error passed as a blocked/reached port")
		}
	}
	for _, text := range []string{"", "COOP_PROBE_BEGIN\n", "COOP_PROBE_BEGIN\nCOOP_PROBE_EXIT 7", "noise\nCOOP_PROBE_BEGIN\nCOOP_PROBE_EXIT 7\n", "COOP_PROBE_BEGIN\nCOOP_PROBE_EXIT 127\n", "COOP_PROBE_BEGIN\nCOOP_PROBE_EXIT 3\n"} {
		if _, err := siblingCurlExit([]byte(text), nil); err == nil {
			t.Fatalf("broken probe accepted: %q", text)
		}
	}
}
