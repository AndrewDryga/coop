package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/fleet"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/ui"
)

const fleetHelp = `coop sessions fleet: manage a local worker fleet

Usage:
  coop sessions fleet serve <state-dir> <listen> <tls-cert> <tls-key>
    [<github-app.json>]
  coop sessions fleet invite <state-dir> <worker> <pool> [--slots-only]
  coop sessions fleet revoke <state-dir> <worker>
  coop sessions fleet submit <state-dir> <pool> <job.json> <prompt-file>
  coop sessions fleet status <state-dir> <job-ref>
  coop sessions fleet workers <state-dir>
  coop sessions fleet cancel <state-dir> <job-ref>
  coop sessions fleet drain <state-dir> <worker>
  coop sessions fleet resume <state-dir> <worker>
  coop sessions fleet limit-account <state-dir> <provider@account> <jobs>
  coop sessions fleet forward <gateway-address> < preview-grant.json
  coop sessions fleet artifacts <job-uuid> <output-dir> [file ...] < checkpoint
  coop sessions fleet checkout <job-uuid> <repo> <output-dir> < checkpoint
  coop sessions fleet snapshot <job-uuid> <repo-ref> <repo> <base> [file ...]
  coop sessions fleet events <state-dir> <job-ref> [<after-sequence>]

COMMANDS
  serve          Listen for outbound worker connections
  invite         Issue a one-time worker invitation
  revoke         End a worker's access
  submit         Queue an immutable job and prompt
  status         Read the job's state and result
  workers        List enrolled workers and capacity
  cancel         Request cancellation of a job
  drain          Stop assigning work to a worker
  resume         Allow a worker to accept work
  limit-account  Set fleet-wide account concurrency
  events         Read retained job events
  forward        Open a private local HTTP preview of a Ryker job service
  snapshot       Write tracked changes and selected untracked files to stdout
  artifacts      List or retrieve saved files from the task artifacts directory
  checkout       Restore an exported Ryker job into a fresh local directory

STATE
  Worker identities and job results stay in the private state directory.
  Invitations work once, within one hour. Deliver them privately.
  The TLS certificate must match the controller hostname workers connect to.`

var fleetCommands = []string{"serve", "invite", "revoke", "submit", "status", "workers", "cancel", "drain", "resume", "limit-account", "events", "checkout", "snapshot", "forward", "artifacts"}

func parseFleetArgs(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		return nil
	}
	count, ok := map[string]int{"artifacts": 3, "forward": 2, "serve": 5, "invite": 4, "revoke": 3, "submit": 5, "status": 3, "workers": 2, "cancel": 3, "drain": 3, "resume": 3, "limit-account": 4, "events": 3, "checkout": 4, "snapshot": 5}[args[0]]
	if !ok {
		if strings.HasPrefix(args[0], "-") {
			return ui.UnknownOption(args[0], "coop sessions fleet", "")
		}
		return unknownSubcommandErr("sessions fleet", args[0], fleetCommands)
	}
	if len(args) != count && !(args[0] == "snapshot" && len(args) >= 5) && !(args[0] == "artifacts" && len(args) >= 3) && !(args[0] == "serve" && len(args) == 6) && !(args[0] == "events" && len(args) == 4) && !(args[0] == "invite" && len(args) == 5 && args[4] == "--slots-only") {
		return &ui.UsageError{Headline: "Invalid arguments for coop sessions fleet " + args[0], Rows: [][2]string{{"Help:", "coop sessions fleet --help"}}}
	}
	return nil
}

var fleetJobUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func runFleet(args []string) (int, error) {
	if len(args) == 0 || args[0] == "--help" {
		fmt.Println(fleetHelp)
		return 0, nil
	}
	if err := parseFleetArgs(args); err != nil {
		return 2, err
	}
	if args[0] == "forward" {
		if err := runFleetPreview(args[1], os.Stdin, os.Stdout); err != nil {
			return 1, err
		}
		return 0, nil
	}
	if args[0] == "checkout" || args[0] == "snapshot" || args[0] == "artifacts" {
		id := strings.ToLower(args[1])
		if !fleetJobUUID.MatchString(id) {
			return 2, ui.InvalidOptionValue("", "job-uuid", "coop sessions fleet "+args[0], "Use the job UUID printed by fleet-submit", "")
		}
		if args[0] == "artifacts" {
			report, err := sessionsvc.FleetResultArtifacts(context.Background(), "operator-fleet-job:"+id, args[2], args[3:], os.Stdin)
			if err != nil {
				return 1, err
			}
			return 0, json.NewEncoder(os.Stdout).Encode(report)
		}
		if args[0] == "snapshot" {
			if ui.IsTerminal(os.Stdout) {
				return 2, &ui.UsageError{Headline: "Redirect the fleet snapshot into a private file or pipe"}
			}
			_, err := sessionsvc.CaptureFleetWorkspace(context.Background(), args[3], "operator-fleet-job:"+id, args[2], args[4], args[5:], os.Stdout)
			if err != nil {
				return 1, err
			}
			return 0, nil
		}
		path, err := sessionsvc.CheckoutFleetResult(context.Background(), "operator-fleet-job:"+id, args[2], args[3], os.Stdin)
		if err != nil {
			return 1, err
		}
		fmt.Println(path)
		return 0, nil
	}
	c, err := fleet.Open(args[1])
	if err != nil {
		return 1, err
	}
	defer c.Close()
	switch args[0] {
	case "events":
		var after int64
		if len(args) == 4 {
			after, err = strconv.ParseInt(args[3], 10, 64)
			if err != nil || after < 0 {
				return 2, ui.InvalidOptionValue("", "after-sequence", "coop sessions fleet events", "Use a nonnegative integer", "")
			}
		}
		err = c.Events(args[2], after, os.Stdout)
	case "limit-account":
		permits, e := strconv.Atoi(args[3])
		if e != nil {
			return 2, ui.InvalidOptionValue("", "parallel-jobs", "coop sessions fleet limit-account", "Use a whole number", "")
		}
		err = c.SetAccountLimit(args[2], permits)
	case "cancel":
		err = c.Cancel(args[2])
	case "drain", "resume":
		err = c.Drain(args[2], args[0] == "drain")
	case "workers":
		var workers []fleet.Worker
		workers, err = c.Workers()
		if err == nil {
			for _, worker := range workers {
				if err = json.NewEncoder(os.Stdout).Encode(worker); err != nil {
					break
				}
			}
		}
	case "serve":
		if len(args) == 6 {
			raw, e := fleetInput(args[5])
			if e != nil {
				return 1, e
			}
			var app fleet.GitHubApp
			if e = json.Unmarshal(raw, &app); e != nil {
				return 2, e
			}
			if e = c.UseGitHubApp(app); e != nil {
				return 1, e
			}
		}
		err = c.Server(args[2]).ListenAndServeTLS(args[3], args[4])
	case "invite":
		var token string
		token, err = c.Invite(args[2], args[3], len(args) == 5)
		if err == nil {
			fmt.Println(token)
		}
	case "revoke":
		err = c.Revoke(args[2])
	case "submit":
		var job, prompt []byte
		job, err = fleetInput(args[3])
		if err != nil {
			return 1, err
		}
		prompt, err = fleetInput(args[4])
		if err != nil {
			return 1, err
		}
		var id string
		id, err = c.Submit(args[2], job, string(prompt))
		if err == nil {
			fmt.Println(id)
		}
	case "status":
		var status fleet.JobStatus
		status, err = c.Status(args[2])
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(status)
		}
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func fleetInput(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const limit = 256 << 10
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(data) > limit {
		return nil, errors.New("fleet input exceeds 256 KiB")
	}
	return data, err
}
