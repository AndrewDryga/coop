package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestQueueArgumentValidationAcrossQueues(t *testing.T) {
	for _, tc := range []struct {
		family string
		args   []string
	}{
		{"backlog", []string{"ls", "--json"}},
		{"backlog", []string{"--json"}},
		{"backlog", []string{"ls", "unexpected"}},
		{"backlog", []string{"promote", "absent", "--bogus"}},
		{"backlog", []string{"promote", "absent", "extra"}},
		{"backlog", []string{"rm", "absent", "--bogus"}},
		{"tasks", []string{"ls", "--json"}},
		{"tasks", []string{"lint", "--json"}},
		{"tasks", []string{"lint", "unexpected"}},
		{"tasks", []string{"release", "absent", "--bogus"}},
		{"tasks", []string{"path", "absent", "extra"}},
		{"tasks", []string{"done", "absent", "--bogus"}},
	} {
		for _, count := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/%d queues/%s", tc.family, count, strings.Join(tc.args, " ")), func(t *testing.T) {
				cfg := validationQueues(t, count)
				out := captureStdout(t, func() {
					var code int
					var err error
					if tc.family == "backlog" {
						code, err = CmdBacklog(cfg, tc.args)
					} else {
						code, err = CmdTasks(Host{}, cfg, tc.args)
					}
					var usage *ui.UsageError
					if code != 2 || !errors.As(err, &usage) {
						t.Errorf("invalid arguments = (%d, %v), want a usage error before listing or lookup", code, err)
					}
				})
				if out != "" {
					t.Errorf("invalid arguments printed a listing: %q", out)
				}
			})
		}
	}
}

func TestQueueMissingItemGuidanceNamesItsFamily(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d queues", count), func(t *testing.T) {
			cfg := validationQueues(t, count)
			for _, family := range []string{"tasks", "backlog"} {
				var code int
				var err error
				if family == "backlog" {
					code, err = CmdBacklog(cfg, []string{"promote", "absent"})
				} else {
					code, err = CmdTasks(Host{}, cfg, []string{"path", "absent"})
				}
				if code != 1 || err == nil || !strings.Contains(err.Error(), "coop "+family) {
					t.Errorf("%s missing item = (%d, %v), want its own listing hint", family, code, err)
				}
			}
		})
	}
}

func validationQueues(t *testing.T, count int) *config.Config {
	t.Helper()
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	cfg := &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir(), TasksFiles: []string{"a/tasks", "b/tasks"}[:count]}
	for _, rel := range cfg.TasksFiles {
		for _, state := range TaskStates {
			if err := os.MkdirAll(filepath.Join(cfg.RepoOverride, rel, state), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	return cfg
}
