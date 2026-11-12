package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// settingsReload is how often a waiting run rereads the settings file
	// for the slot count, which bounds how long a change takes to reach it.
	settingsReload = 2 * time.Second
)

// loadSettings reads the settings file over this machine's defaults. On an
// error the config returned is the defaults.
func loadSettings() (config.Config, error) {
	return config.Load(config.Path(), config.Default(config.ThisMachine()))
}

// settings loads the machine-wide config. A broken file must not block a
// run: verification matters more than the throttle around it. The problem is
// reported and the defaults are used.
func settings(stderr io.Writer) config.Config {
	cfg, err := loadSettings()
	if err != nil && !budget.InScope() { // the outer qrun already reported it
		fmt.Fprintf(stderr, "qrun: settings file ignored, using the defaults (1 slot, half the CPUs, half of RAM):\n%s\n  Fix the keys named above, by hand or with `qrun config set KEY VALUE`; `qrun config check` validates the file.\n", indent(err.Error()))
	}
	return cfg
}

// queue is the machine-wide line under cfg.
func queue(cfg config.Config) gate.Gate {
	return gate.Gate{Dir: gate.DefaultDir(), Slots: cfg.Slots, Poll: gate.DefaultPoll, Hold: cfg.Hold}
}

// liveSlots returns the slot count in force, reread from the settings file
// at most every settingsReload. A file that turns invalid means the
// defaults, as it does for a run that starts then.
func liveSlots(slots int) func() int {
	read := time.Now()
	return func() int {
		if time.Since(read) >= settingsReload {
			read = time.Now()
			cfg, _ := loadSettings() //nolint:errcheck // an invalid file is in force as the defaults, which loadSettings returns
			slots = cfg.Slots
		}
		return slots
	}
}

// throttled runs work under the queue, the budget and, for a run that holds
// a slot, the time limit. argv is this process's
// own arguments: with a budget in force qrun re-executes itself inside the
// budget's scope, and work then runs in that inner process, while this one
// keeps holding the slot.
func throttled(ctx context.Context, stderr io.Writer, title string, noQueue bool, argv []string, work func(context.Context) int) int {
	if os.Getenv(bypassEnv) != "" {
		return work(ctx)
	}
	cfg := settings(stderr)

	if !noQueue && cfg.Slots > 0 && os.Getenv(heldEnv) == "" {
		release, err := waitInLine(ctx, stderr, cfg, title, argv)
		switch {
		case ctx.Err() != nil:
			held := ""
			if err == nil {
				release() // admitted in the instant it was interrupted
			} else if cfg.Hold > 0 {
				held = fmt.Sprintf(" Its place in line is held for %s: run the same command again from this directory to take it back.", config.FormatDuration(cfg.Hold))
			}
			fmt.Fprintf(stderr, "qrun: interrupted while queued; nothing was run.%s\n", held)
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
	// The time limit is on holding a slot, so a run that holds none (no
	// queue, --no-queue) has none: it blocks nobody. The mark is inherited,
	// so the re-executed qrun, which is where the work runs under a budget,
	// applies the limit too.
	if os.Getenv(heldEnv) == "" {
		return work(ctx)
	}
	return limited(ctx, stderr, cfg.TimeLimit, work)
}

// limited runs work, stopping it once it has run for limit. 0 is no limit.
// The deadline's cause is the reason a step shows in its summary.
func limited(ctx context.Context, stderr io.Writer, limit time.Duration, work func(context.Context) int) int {
	if limit <= 0 {
		return work(ctx)
	}
	text := config.FormatDuration(limit)
	ctx, cancel := context.WithTimeoutCause(ctx, limit, fmt.Errorf("timed out at the %s time limit", text))
	defer cancel()

	code := work(ctx)
	// Work that passed keeps its pass even if the deadline came right after.
	if code == exitOK || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return code
	}
	fmt.Fprintf(stderr, "qrun: stopped at the time limit of %s. A run may hold its slot no longer than that, so that a hung command cannot block the line; what had not finished by then did not run.\n  If this work needs longer, raise the limit with `qrun config set time_limit DURATION`, or \"0\" for none.\n", text)
	return exitTimedOut
}

// waitInLine takes a slot, reporting on stderr only if the run has to wait.
// argv is what makes a later run this one's rerun: the same arguments from
// the same directory.
func waitInLine(ctx context.Context, stderr io.Writer, cfg config.Config, title string, argv []string) (release func(), err error) {
	dir, err := os.Getwd()
	if err != nil {
		dir = "(unknown directory)"
	}
	start := time.Now()
	// A digest, not the arguments: a command line can carry a secret, and
	// this is written to the queue and shown by `qrun status --json`.
	digest := sha256.Sum256([]byte(strings.Join(argv, "\x00")))
	who := gate.Entry{Title: title, Dir: dir, PID: os.Getpid(), Since: start.UTC(), Fingerprint: hex.EncodeToString(digest[:8])}
	g := queue(cfg)
	g.Reload = liveSlots(cfg.Slots)

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
					// Naming the holder each time shows a waiting caller a run
					// that has been holding the slot for far too long.
					var running []gate.Entry
					if st, err := g.Status(); err == nil {
						running = st.Running
					}
					fmt.Fprintf(stderr, "qrun: still queued after %s, position %d%s\n", steps.Dur(time.Since(start)), position, runningNow(running, time.Now()))
				}
				mu.Unlock()
			}
		}
	}()

	release, err = g.Acquire(ctx, who, func(w gate.Wait) {
		mu.Lock()
		defer mu.Unlock()
		if position == 0 {
			place, ifCancelled := "", "Cancelling loses this place in line."
			if w.Rejoined {
				place = ", the place held from an earlier attempt"
			}
			if cfg.Hold > 0 {
				ifCancelled = fmt.Sprintf("If cancelled, this place is held for %s for the same command run again.", config.FormatDuration(cfg.Hold))
			}
			fmt.Fprintf(stderr, "qrun: queued at position %d%s%s. %s\n", w.Position, place, runningNow(w.Running, start), ifCancelled)
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
