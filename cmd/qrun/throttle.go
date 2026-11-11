package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sustinbebustin/qrun/internal/budget"
	"github.com/sustinbebustin/qrun/internal/config"
	"github.com/sustinbebustin/qrun/internal/gate"
	"github.com/sustinbebustin/qrun/internal/steps"
)

const (
	// heldEnv marks a process tree that already holds a slot, so a step that
	// itself calls qrun does not queue behind its own parent.
	heldEnv = "QRUN_HOLDS_SLOT"
	// bypassEnv runs with no queue and no budget: the escape hatch for a
	// person who needs a result now.
	bypassEnv = "QRUN_BYPASS"

	// waitHeartbeat is how often a queued run repeats its position, so a
	// caller watching a quiet stream can tell waiting from hanging.
	waitHeartbeat = 30 * time.Second
	// stopGrace is how long a child gets to wind down after being asked to
	// stop before qrun stops waiting on it. It exceeds the steps package's
	// own grace so an inner qrun can finish reporting first.
	stopGrace = 15 * time.Second
)

// settings loads the machine-wide config. A broken file must not block a
// run: verification matters more than the throttle around it. The problem is
// reported and the defaults are used.
func settings(stderr io.Writer) config.Config {
	def := config.Default(config.ThisMachine())
	cfg, err := config.Load(config.Path(), def)
	if err != nil && !budget.InScope() { // the outer qrun already reported it
		fmt.Fprintf(stderr, "qrun: settings file ignored, using the defaults (1 slot, half the CPUs, half of RAM):\n%s\n  Fix the keys named above; `qrun status` shows the values in force.\n", indent(err.Error()))
	}
	return cfg
}

// throttled runs work under the queue and the budget. argv is this process's
// own arguments: with a budget in force qrun re-executes itself inside the
// budget's scope, and work then runs in that inner process, while this one
// keeps holding the slot.
func throttled(ctx context.Context, stderr io.Writer, title string, noQueue bool, argv []string, work func(context.Context) int) int {
	if os.Getenv(bypassEnv) != "" {
		return work(ctx)
	}
	cfg := settings(stderr)

	if !noQueue && cfg.Slots > 0 && os.Getenv(heldEnv) == "" {
		release, err := waitInLine(ctx, stderr, cfg.Slots, title)
		switch {
		case ctx.Err() != nil:
			fmt.Fprintln(stderr, "qrun: interrupted while queued; nothing was run")
			return exitInterrupted
		case err != nil:
			fmt.Fprintf(stderr, "qrun: queue unavailable, running without waiting in line: %v\n", err)
		default:
			defer release()
			// Inherited by the re-executed qrun and by every step.
			if err := os.Setenv(heldEnv, "1"); err != nil {
				fmt.Fprintf(stderr, "qrun: cannot mark the slot as held (%v); a step that calls qrun will wait for this run and never start\n", err)
			}
		}
	}

	if cfg.Budget.Enabled && !budget.InScope() && budget.Available() {
		if err := budget.Apply(ctx, cfg.Budget); err != nil {
			fmt.Fprintf(stderr, "qrun: budget not applied, running without CPU or memory limits: %v\n", err)
		} else if code, started := reexecInBudget(ctx, argv); started {
			return code
		} else {
			fmt.Fprintln(stderr, "qrun: could not start inside the budget (systemd-run failed to launch), running without CPU or memory limits")
		}
	}
	return work(ctx)
}

// waitInLine takes a slot, reporting on stderr only if the run has to wait.
func waitInLine(ctx context.Context, stderr io.Writer, slots int, title string) (release func(), err error) {
	dir, err := os.Getwd()
	if err != nil {
		dir = "(unknown directory)"
	}
	start := time.Now()
	who := gate.Entry{Title: title, Dir: dir, PID: os.Getpid(), Since: start.UTC()}
	g := gate.Gate{Dir: gate.DefaultDir(), Slots: slots, Poll: gate.DefaultPoll}

	var (
		mu       sync.Mutex
		position int
	)
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(waitHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				mu.Lock()
				if position > 0 {
					fmt.Fprintf(stderr, "qrun: still queued after %s, position %d\n", steps.Dur(time.Since(start)), position)
				}
				mu.Unlock()
			}
		}
	}()

	release, err = g.Acquire(ctx, who, func(w gate.Wait) {
		mu.Lock()
		defer mu.Unlock()
		if position == 0 {
			fmt.Fprintf(stderr, "qrun: queued at position %d%s. Cancelling loses this place in line.\n", w.Position, runningNow(w.Running, start))
		} else {
			fmt.Fprintf(stderr, "qrun: now at position %d\n", w.Position)
		}
		position = w.Position
	})
	mu.Lock()
	waited := position > 0
	position = 0 // silences the heartbeat
	mu.Unlock()
	if err == nil && waited {
		fmt.Fprintf(stderr, "qrun: started after %s in line\n", steps.Dur(time.Since(start)))
	}
	return release, err
}

// runningNow describes the runs holding the slots, for a run that must wait.
func runningNow(running []gate.Entry, now time.Time) string {
	if len(running) == 0 {
		return ""
	}
	parts := make([]string, len(running))
	for i, e := range running {
		parts[i] = fmt.Sprintf("%s in %s (pid %d, %s)", e.Title, tildePath(e.Dir), e.PID, steps.Dur(now.Sub(e.Since)))
	}
	return "; running now: " + strings.Join(parts, ", ")
}

// reexecInBudget runs this same qrun invocation inside the budget's scope and
// returns its exit code. started is false when it could not be launched.
func reexecInBudget(ctx context.Context, argv []string) (code int, started bool) {
	self, err := os.Executable()
	if err != nil {
		return 0, false
	}
	cmd := budget.Command(ctx, self, argv)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return supervise(cmd)
}

// supervise runs a child that shares this process's terminal and returns its
// exit code, 128+signal if a signal killed it. When this process is asked to
// stop, the child is asked first and given stopGrace to finish.
func supervise(cmd *exec.Cmd) (code int, started bool) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopGrace
	if err := cmd.Start(); err != nil {
		return 0, false
	}
	err := cmd.Wait()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), true
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) && cmd.ProcessState.ExitCode() == 0 {
		// The child exited 0 but its output could not be drained in time.
		return exitOK, true
	}
	return cmd.ProcessState.ExitCode(), true
}

// tildePath shortens a path under the home directory to ~/..., for messages.
func tildePath(path string) string {
	home := os.Getenv("HOME")
	if home == "" {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		if rel == "." {
			return "~"
		}
		return filepath.Join("~", rel)
	}
	return path
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
