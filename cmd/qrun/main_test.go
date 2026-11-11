package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sustinbebustin/qrun/internal/gate"
)

func invoke(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestParseRunArgs(t *testing.T) {
	t.Run("single step takes its title from the label", func(t *testing.T) {
		a, err := parseRunArgs([]string{"--step", "lint=pnpm lint"})
		if err != nil {
			t.Fatal(err)
		}
		if a.opts.Title != "lint" {
			t.Errorf("title = %q, want lint", a.opts.Title)
		}
		if len(a.steps) != 1 || a.steps[0].Cmd != "pnpm lint" {
			t.Errorf("steps = %+v", a.steps)
		}
	})

	t.Run("command may contain equals signs", func(t *testing.T) {
		a, err := parseRunArgs([]string{"--step", "checks=turbo run lint --output-logs=errors-only"})
		if err != nil {
			t.Fatal(err)
		}
		if want := "turbo run lint --output-logs=errors-only"; a.steps[0].Cmd != want {
			t.Errorf("cmd = %q, want %q", a.steps[0].Cmd, want)
		}
	})

	t.Run("multiple steps keep their order and the explicit title", func(t *testing.T) {
		a, err := parseRunArgs([]string{"--title", "verify-go", "--step", "lint=golangci-lint run", "--step", "test=go test ./..."})
		if err != nil {
			t.Fatal(err)
		}
		if a.opts.Title != "verify-go" || len(a.steps) != 2 || a.steps[0].Label != "lint" || a.steps[1].Label != "test" {
			t.Errorf("args = %+v", a)
		}
	})

	t.Run("flags toggle modes", func(t *testing.T) {
		a, err := parseRunArgs([]string{"--show-on-success", "--fail-nonempty", "--no-queue", "--log-dir", "/logs", "--step", "x=true"})
		if err != nil {
			t.Fatal(err)
		}
		if !a.opts.ShowOnSuccess || !a.opts.FailNonempty || !a.noQueue || a.opts.LogDir != "/logs" {
			t.Errorf("args = %+v", a)
		}
	})

	t.Run("the default log dir is per user and per working directory", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("TMPDIR", "/scratch")
		logDirIn := func(checkout string) string {
			t.Helper()
			dir := filepath.Join(root, checkout, "app")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			a, err := parseRunArgs([]string{"--step", "x=true"})
			if err != nil {
				t.Fatal(err)
			}
			return a.opts.LogDir
		}

		one, two := logDirIn("worktree-1"), logDirIn("worktree-2")
		prefix := fmt.Sprintf("/scratch/qrun-%d/logs/app-", os.Getuid())
		if !strings.HasPrefix(one, prefix) {
			t.Errorf("log dir = %q, want prefix %q", one, prefix)
		}
		// Worktrees of one repository share a directory name.
		if one == two {
			t.Errorf("two checkouts named alike share %q; one run's log would overwrite the other's", one)
		}
		if again := logDirIn("worktree-1"); again != one {
			t.Errorf("log dir changed between runs: %q then %q", one, again)
		}
	})

	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"missing value", []string{"--step"}},
		{"malformed step", []string{"--step", "noequals"}},
		{"empty label", []string{"--step", "=cmd"}},
		{"empty command", []string{"--step", "label= "}},
		{"unknown flag", []string{"--bogus"}},
		{"no steps", []string{"--title", "verify"}},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if a, err := parseRunArgs(tc.argv); err == nil {
				t.Errorf("parseRunArgs(%v) = %+v, want an error", tc.argv, a)
			}
		})
	}
}

func TestParseExecArgs(t *testing.T) {
	t.Run("command after the separator keeps its own flags", func(t *testing.T) {
		a, err := parseExecArgs([]string{"--", "go", "test", "--title", "x", "-race"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(a.command, []string{"go", "test", "--title", "x", "-race"}) || a.title != "go test" {
			t.Errorf("args = %+v", a)
		}
	})

	t.Run("the separator is optional when the command comes first", func(t *testing.T) {
		a, err := parseExecArgs([]string{"--no-queue", "gofmt", "-l", "."})
		if err != nil {
			t.Fatal(err)
		}
		if !a.noQueue || !slices.Equal(a.command, []string{"gofmt", "-l", "."}) {
			t.Errorf("args = %+v", a)
		}
	})

	// The title is what `qrun status` and waiting runs show, so "go" alone
	// would not tell a test run from a vet.
	t.Run("title defaults to the command's name and subcommand", func(t *testing.T) {
		for _, tc := range []struct {
			command []string
			want    string
		}{
			{[]string{"/usr/local/bin/golangci-lint", "run", "./..."}, "golangci-lint run"},
			{[]string{"go", "test", "-race", "./..."}, "go test"},
			{[]string{"go", "-C", "backend", "test"}, "go"},
			{[]string{"sh", "-c", "make all"}, "sh"},
			{[]string{"pytest"}, "pytest"},
			{[]string{"pytest", "tests/unit/test_api.py"}, "pytest"},
		} {
			a, err := parseExecArgs(append([]string{"--"}, tc.command...))
			if err != nil {
				t.Fatal(err)
			}
			if a.title != tc.want {
				t.Errorf("title of %v = %q, want %q", tc.command, a.title, tc.want)
			}
		}
		a, err := parseExecArgs([]string{"--title", "lint", "--", "golangci-lint"})
		if err != nil {
			t.Fatal(err)
		}
		if a.title != "lint" {
			t.Errorf("title = %q, want the explicit one", a.title)
		}
	})

	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"no command", nil},
		{"separator with no command", []string{"--"}},
		{"unknown flag before the command", []string{"--fast", "go", "test"}},
		{"title without a value", []string{"--title"}},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if a, err := parseExecArgs(tc.argv); err == nil {
				t.Errorf("parseExecArgs(%v) = %+v, want an error", tc.argv, a)
			}
		})
	}
}

func TestUsageErrorsShowACorrectInvocation(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--step", "noequals"},
		{"exec"},
		{"status", "--bogus"},
	} {
		code, stdout, stderr := invoke(args...)
		if code != exitUsage {
			t.Errorf("qrun %v: exit %d, want %d", args, code, exitUsage)
		}
		if stdout != "" {
			t.Errorf("qrun %v: wrote to stdout: %q", args, stdout)
		}
		if !strings.Contains(stderr, "example: qrun ") || !strings.Contains(stderr, "help:    qrun ") {
			t.Errorf("qrun %v: error lacks an example or a help pointer:\n%s", args, stderr)
		}
	}
}

func TestEveryCommandHasHelpWithExamples(t *testing.T) {
	for _, args := range [][]string{
		{"--help"}, {"-h"}, {"help"},
		{"--step", "a=b", "--help"},
		{"exec", "--help"},
		{"status", "--help"},
	} {
		code, stdout, _ := invoke(args...)
		if code != exitOK {
			t.Errorf("qrun %v: exit %d", args, code)
		}
		if !strings.Contains(stdout, "Usage:") || !strings.Contains(stdout, "Examples:") {
			t.Errorf("qrun %v: help lacks Usage or Examples:\n%s", args, stdout)
		}
	}
}

func TestVersion(t *testing.T) {
	code, stdout, _ := invoke("version")
	if code != exitOK || stdout != "qrun "+version+"\n" {
		t.Errorf("exit %d, stdout %q", code, stdout)
	}
}

func TestExecOfAMissingCommandFailsBeforeQueueing(t *testing.T) {
	code, _, stderr := invoke("exec", "--", "qrun-test-no-such-command")
	if code != exitNotFound {
		t.Errorf("exit %d, want %d", code, exitNotFound)
	}
	if !strings.Contains(stderr, "qrun-test-no-such-command") || !strings.Contains(stderr, "Nothing was run") {
		t.Errorf("stderr = %q", stderr)
	}
}

func renderedStatus(r statusReport) string {
	var b bytes.Buffer
	renderStatus(&b, r, time.Unix(1_700_000_100, 0))
	return b.String()
}

func TestRenderStatus(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	entry := func(title string, agoSeconds int64) gate.Entry {
		return gate.Entry{Title: title, Dir: "/home/someone/src/" + title, PID: 42, Since: time.Unix(1_700_000_100-agoSeconds, 0)}
	}
	enforced := budgetReport{Enabled: true, Enforced: true, CPUs: 11, MemoryMax: "12G", Slice: "qrun.slice"}
	settings := configReport{Path: "/home/someone/.config/qrun/config.toml"}

	t.Run("idle", func(t *testing.T) {
		got := renderedStatus(statusReport{Slots: 1, Budget: enforced, Config: settings})
		want := "running: none (1 slot free)\n" +
			"budget: 11 CPUs, memory capped at 12G with swap off, shared by all runs (qrun.slice)\n" +
			"settings: defaults (create /home/someone/.config/qrun/config.toml to change them)\n"
		if got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("busy lists the line in order", func(t *testing.T) {
		got := renderedStatus(statusReport{
			Slots:   1,
			Running: []gate.Entry{entry("verify", 95)},
			Waiting: []gate.Entry{entry("lint", 30), entry("test", 5)},
			Budget:  enforced,
			Config:  configReport{Path: settings.Path, Exists: true},
		})
		for _, want := range []string{
			"running (1 of 1 slot):\n  verify  in ~/src/verify  pid 42  for 1m35s\n",
			"waiting (2), next first:\n  1. lint  in ~/src/lint  pid 42  waited 30.0s\n  2. test  in ~/src/test  pid 42  waited 5.0s\n",
			"settings: /home/someone/.config/qrun/config.toml\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
	})

	t.Run("says when a limit is not in force", func(t *testing.T) {
		got := renderedStatus(statusReport{Slots: 0, Budget: budgetReport{Enabled: true}, Config: settings})
		if !strings.Contains(got, "queue: off") || !strings.Contains(got, "budget: not enforced on this machine") {
			t.Errorf("got:\n%s", got)
		}
		got = renderedStatus(statusReport{Slots: 1, Budget: budgetReport{}, Config: settings})
		if !strings.Contains(got, "budget: off") {
			t.Errorf("got:\n%s", got)
		}
	})

	t.Run("an invalid settings file is called out with its error", func(t *testing.T) {
		got := renderedStatus(statusReport{Slots: 1, Budget: enforced, Config: configReport{Path: settings.Path, Exists: true, Error: settings.Path + ":\n  slots = -1: must be 0 (no queue) or more"}})
		if !strings.Contains(got, "settings: invalid, so the defaults are in force:\n  "+settings.Path+":\n    slots = -1") {
			t.Errorf("got:\n%s", got)
		}
	})
}

func TestTildePath(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	for path, want := range map[string]string{
		"/home/someone":         "~",
		"/home/someone/src/app": "~/src/app",
		"/home/someoneelse/app": "/home/someoneelse/app",
		"/srv/app":              "/srv/app",
	} {
		if got := tildePath(path); got != want {
			t.Errorf("tildePath(%q) = %q, want %q", path, got, want)
		}
	}
}
