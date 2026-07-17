//go:build cooplivetest

package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/AndrewDryga/coop/internal/liveprocess"
	"github.com/AndrewDryga/coop/internal/processidentity"
)

const liveACPStartGate = `
IFS= read -r coop_gate <&3 || exit 125
[ "$coop_gate" = go ] || exit 125
exec 3<&-
trap '' TERM HUP INT
# A non-interactive shell gives an asynchronous command /dev/null as stdin. Preserve the editor's
# ACP pipe on a private descriptor before backgrounding, then close the duplicate in both processes.
exec 4<&0
( trap - TERM HUP INT; exec "$@" <&4 4<&- ) &
coop_child=$!
exec 4<&-
exec 0<&- 1>&- 2>&-
wait "$coop_child"
while :; do /bin/sleep 3600; done
`

var (
	readLiveACPStartToken = processidentity.StartToken
	beforeLiveACPRelease  = func() {}
	liveACPRegistryMu     sync.Mutex
)

// startACPProcess is compiled only into isolated live-test binaries. The resident shell wrapper is
// the generation's stable PGID leader. It cannot exec the inner Coop until its authenticated record
// is durably published; losing the outer process closes the gate and fails before exec.
func startACPProcess(cmd *exec.Cmd, supervisor string) error {
	registry, cleanupID, enabled, err := openLiveACPRegistry()
	if err != nil {
		return err
	}
	if !enabled {
		return cmd.Start()
	}
	defer registry.Close()
	if supervisor == "" || len(cmd.ExtraFiles) != 0 {
		return errors.New("invalid live ACP process contract")
	}

	gateR, gateW, err := os.Pipe()
	if err != nil {
		return errors.New("create live ACP start gate")
	}
	original := append([]string{cmd.Path}, cmd.Args[1:]...)
	cmd.Path = "/bin/sh"
	cmd.Args = append([]string{"sh", "-c", liveACPStartGate, "coop-live-acp-gate"}, original...)
	cmd.ExtraFiles = []*os.File{gateR}
	if err := cmd.Start(); err != nil {
		gateR.Close()
		gateW.Close()
		return err
	}
	gateR.Close()
	abort := func(cause error) error {
		gateW.Close() // EOF keeps a not-yet-released wrapper from starting the inner command.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		return cause
	}

	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid != pid {
		return abort(errors.New("verify live ACP process group"))
	}
	token := readLiveACPStartToken(pid)
	if !processidentity.Stable(token) {
		return abort(errors.New("read live ACP process identity"))
	}
	record := liveprocess.Record{
		Schema: liveprocess.RecordSchema, PID: pid, PGID: pgid, UID: os.Getuid(),
		StartToken: token, Supervisor: cleanupID,
	}
	if err := publishLiveACPRecord(registry, record); err != nil {
		return abort(err)
	}
	beforeLiveACPRelease()
	if _, err := io.WriteString(gateW, "go\n"); err != nil {
		return abort(errors.New("release live ACP start gate"))
	}
	if err := gateW.Close(); err != nil {
		return abort(errors.New("close live ACP start gate"))
	}
	return nil
}

func openLiveACPRegistry() (*os.Root, string, bool, error) {
	rawFD := os.Getenv(liveprocess.ControlFDEnv)
	processDir := os.Getenv(liveprocess.ProcessDirEnv)
	cleanupID := os.Getenv(liveprocess.CleanupIDEnv)
	if rawFD == "" && processDir == "" && cleanupID == "" {
		return nil, "", false, nil
	}
	fd, err := strconv.Atoi(rawFD)
	if err != nil || fd != 3 || !liveprocess.ValidateControlFD(fd) || !liveprocess.ValidCleanupID(cleanupID) {
		return nil, "", false, errors.New("invalid live process control")
	}
	stateRoot := filepath.Clean(os.Getenv("XDG_STATE_HOME"))
	if !filepath.IsAbs(stateRoot) || filepath.Dir(processDir) != stateRoot ||
		filepath.Base(processDir) != "live-process-groups" {
		return nil, "", false, errors.New("invalid live process registry")
	}
	rootInfo, err := os.Lstat(stateRoot)
	if err != nil || !privateOwnedDirectory(rootInfo) {
		return nil, "", false, errors.New("invalid live process registry")
	}
	dirInfo, err := os.Lstat(processDir)
	if err != nil || !privateOwnedDirectory(dirInfo) {
		return nil, "", false, errors.New("invalid live process registry")
	}
	state, err := os.OpenRoot(stateRoot)
	if err != nil {
		return nil, "", false, errors.New("open live process registry root")
	}
	registry, err := state.OpenRoot(filepath.Base(processDir))
	state.Close()
	if err != nil {
		return nil, "", false, errors.New("open live process registry")
	}
	opened, err := registry.Stat(".")
	if err != nil || !os.SameFile(dirInfo, opened) {
		registry.Close()
		return nil, "", false, errors.New("live process registry changed while opening")
	}
	return registry, cleanupID, true, nil
}

func privateOwnedDirectory(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func publishLiveACPRecord(registry *os.Root, record liveprocess.Record) error {
	liveACPRegistryMu.Lock()
	defer liveACPRegistryMu.Unlock()
	directory, err := registry.Open(".")
	if err != nil {
		return errors.New("open live ACP process registry")
	}
	entries, readErr := directory.ReadDir(liveprocess.MaxRecords + 1)
	closeErr := directory.Close()
	if (readErr != nil && !errors.Is(readErr, io.EOF)) || closeErr != nil || len(entries) >= liveprocess.MaxPublishedRecords {
		return errors.New("live ACP process registry is full")
	}

	data, err := json.Marshal(record)
	if err != nil || len(data) > liveprocess.MaxRecordSize-1 {
		return errors.New("encode live ACP process record")
	}
	data = append(data, '\n')
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errors.New("create live ACP process record name")
	}
	base := strconv.Itoa(record.PID) + "-" + hex.EncodeToString(nonce[:])
	pending, final := ".pending-"+base, base+".json"
	file, err := registry.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create live ACP process record")
	}
	keep := false
	defer func() {
		file.Close()
		if !keep {
			_ = registry.Remove(pending)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return errors.New("secure live ACP process record")
	}
	if _, err := file.Write(data); err != nil {
		return errors.New("write live ACP process record")
	}
	if err := file.Sync(); err != nil {
		return errors.New("sync live ACP process record")
	}
	if err := file.Close(); err != nil {
		return errors.New("close live ACP process record")
	}
	if err := registry.Link(pending, final); err != nil {
		return errors.New("publish live ACP process record")
	}
	if err := registry.Remove(pending); err != nil {
		return errors.New("finalize live ACP process record")
	}
	directory, err = registry.Open(".")
	if err != nil {
		return errors.New("open live ACP process registry for sync")
	}
	syncErr := directory.Sync()
	syncCloseErr := directory.Close()
	if syncErr != nil || syncCloseErr != nil {
		return errors.New("sync live ACP process registry")
	}
	keep = true
	return nil
}
