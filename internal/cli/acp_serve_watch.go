package cli

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
)

// serveWatchScript runs inside the live box and reports each server that starts or stops listening on
// a serve port, as one tab-separated line: "up <port> <any|loopback> <command>" or "down <port>". It
// reads the box's own process tables once a second and never connects to anything, because a dev server
// would log a probe. The host holds its stdin open: closing it ends the script, and the box stopping
// ends it anyway. The arguments are the serve ports.
const serveWatchScript = `proc=${COOP_SERVE_WATCH_PROC:-/proc}
exec 3<&0
(cat <&3 >/dev/null; kill $$ 2>/dev/null) &
known=""
command_of() {
	for dir in "$proc"/[0-9]*; do
		if ls -l "$dir/fd" 2>/dev/null | grep -qF "socket:[$1]"; then
			tr '\000\n\t' '   ' <"$dir/cmdline" 2>/dev/null | cut -c1-200
			return
		fi
	done
}
while :; do
	now=""
	for file in "$proc/net/tcp" "$proc/net/tcp6"; do
		[ -r "$file" ] || continue
		while read -r _ local _ state _ _ _ _ _ inode _; do
			[ "$state" = 0A ] || continue
			for port in "$@"; do
				[ "${local##*:}" = "$(printf %04X "$port")" ] || continue
				case "${local%:*}" in
				*0100007F | 00000000000000000000000001000000) scope=loopback ;;
				*) scope=any ;;
				esac
				now="$now $port:$scope:$inode"
			done
		done <"$file"
	done
	for entry in $now; do
		case " $known " in *" $entry "*) continue ;; esac
		port=${entry%%:*}
		rest=${entry#*:}
		printf 'up\t%s\t%s\t%s\n' "$port" "${rest%%:*}" "$(command_of "${rest#*:}")" || exit 0
	done
	for entry in $known; do
		port=${entry%%:*}
		case " $now " in *" $port:"*) continue ;; esac
		printf 'down\t%s\n' "$port" || exit 0
	done
	known=$now
	sleep 1
done
`

// serveEvent is one line of the watcher. It comes from inside a box, so it is parsed strictly, only a
// session's own serve ports count, and the command is treated as foreign text wherever it is shown.
type serveEvent struct {
	port     int
	up       bool
	loopback bool
	command  string
}

func parseServeEvent(line string) (serveEvent, bool) {
	fields := strings.SplitN(line, "\t", 4)
	if len(fields) < 2 {
		return serveEvent{}, false
	}
	port, err := strconv.Atoi(fields[1])
	if err != nil || port < 1 || port > 65535 {
		return serveEvent{}, false
	}
	switch {
	case fields[0] == "down" && len(fields) == 2:
		return serveEvent{port: port}, true
	case fields[0] == "up" && len(fields) == 4 && (fields[2] == "any" || fields[2] == "loopback"):
		return serveEvent{port: port, up: true, loopback: fields[2] == "loopback", command: strings.TrimSpace(fields[3])}, true
	}
	return serveEvent{}, false
}

// serveAnnouncer turns watcher lines into what a thread reads: each server start once, however it
// listens, and a box writing lines of its own gets one message a port every two seconds at most.
type serveAnnouncer struct {
	ports  func() []acpctl.ServePort
	notify func(string)
	now    func() time.Time

	up   map[int]bool // loopback listeners count as up, so a later any-address bind still speaks
	said map[int]time.Time
}

func (s *serveAnnouncer) handle(event serveEvent) {
	if s.up == nil {
		s.up, s.said = map[int]bool{}, map[int]time.Time{}
	}
	if !event.up {
		delete(s.up, event.port)
		return
	}
	var port acpctl.ServePort
	found := false
	for _, p := range s.ports() {
		if p.Box == event.port {
			port, found = p, true
		}
	}
	if !found || s.up[event.port] && event.loopback {
		return
	}
	if now := s.now(); now.Sub(s.said[event.port]) >= 2*time.Second {
		s.said[event.port] = now
		if event.loopback {
			s.notify(acpctl.ServeLoopbackText(port, event.command))
		} else {
			s.notify(acpctl.ServeUpText(port, event.command))
		}
	}
	s.up[event.port] = true
}

// follow watches the box the thread now uses, and stops watching the one before it. find names the
// box's container once it has one; a box that never gets one, such as a holding child, is left alone.
func (r *acpServeRouter) follow(find func(context.Context) (string, error)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopWatch != nil {
		r.stopWatch()
		r.stopWatch = nil
	}
	if find == nil || r.execBox == nil || r.closed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.stopWatch = cancel
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer cancel()
		if err := r.watchBox(ctx, find); err != nil && ctx.Err() == nil {
			acpproxy.Trace("serve watch: %v", err)
		}
	}()
}

func (r *acpServeRouter) watchBox(ctx context.Context, find func(context.Context) (string, error)) error {
	container := ""
	for deadline := time.Now().Add(3 * time.Minute); container == ""; {
		id, err := find(ctx)
		if err == nil {
			container = id
		}
		if container != "" {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("the box never appeared")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	argv := []string{"sh", "-c", serveWatchScript, "coop-serve-watch"}
	for _, port := range r.Ports() {
		argv = append(argv, strconv.Itoa(port.Box))
	}
	cmd := r.execBox(ctx, container, argv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	announcer := serveAnnouncer{ports: r.Ports, now: time.Now, notify: func(text string) {
		acpproxy.Trace("serve watch: %s", text)
		r.mu.Lock()
		notify := r.notify
		r.mu.Unlock()
		if notify != nil {
			notify(text)
		}
	}}
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		if event, ok := parseServeEvent(lines.Text()); ok {
			announcer.handle(event)
		}
	}
	var exit *exec.ExitError
	if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
		return err
	}
	return nil
}
