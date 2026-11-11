// Package steps runs labeled shell commands quietly: each step's combined
// output goes to a log file, and the caller gets one summary line on success
// or only the salient error lines plus a pointer to the log on failure.
//
// The goal is a command surface a person or a coding agent can trust without
// reading pages of tool output: run the bare command, read a few lines, never
// pipe, grep or redirect. Nothing is hidden, because the full log is always
// written; the filtering only chooses what to print.
package steps

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Step is one labeled shell command.
type Step struct {
	Label string
	Cmd   string
}

// Result is the outcome of running a Step.
type Result struct {
	Step     Step
	ExitCode int // 128+signal when the step was killed by a signal
	Dur      time.Duration
	Combined string // combined stdout and stderr, ANSI escapes removed
	LogPath  string // empty when the log file could not be created
	OK       bool
	Reason   string // why it failed, when not a plain nonzero exit
}

// Options control how steps are run and reported.
type Options struct {
	// Title names a multi-step run in its summary.
	Title string
	// LogDir receives one <label>.log per step.
	LogDir string
	// ShowOnSuccess prints a passing step's full output, for steps whose
	// output is the point (a generated diff, a report).
	ShowOnSuccess bool
	// FailNonempty fails a step that exits 0 but writes to stdout, for format
	// checks that list offending files and still exit 0 (gofmt -l).
	FailNonempty bool
}

// stopGrace is how long a cancelled step gets to exit after SIGTERM before
// it is killed, and how long a finished step's leftover children may keep its
// output open before qrun stops waiting on them. A variable so tests need
// not wait it out.
var stopGrace = 10 * time.Second

// Run executes steps in order and returns one Result per step that ran. A
// failing step does not stop the run: verification reports every failure at
// once. Cancelling ctx stops the current step and skips the rest.
func Run(ctx context.Context, steps []Step, opts Options) []Result {
	results := make([]Result, 0, len(steps))
	for _, step := range steps {
		if ctx.Err() != nil {
			break
		}
		results = append(results, runStep(ctx, step, opts))
	}
	return results
}

// AllOK reports whether every one of want steps ran and passed.
func AllOK(results []Result, want int) bool {
	if len(results) != want {
		return false
	}
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
}

// runStep executes one step, streaming its combined output to the log file
// while buffering it for filtering, and shows progress on stderr while it
// runs. Color is disabled in the child's environment and leftover ANSI
// escapes are stripped, so logs and summaries stay plain text.
func runStep(ctx context.Context, step Step, opts Options) Result {
	logPath := filepath.Join(opts.LogDir, sanitize(step.Label)+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		logPath = ""
	}

	var combined, stdoutOnly bytes.Buffer
	all := io.Writer(&combined)
	if logFile != nil {
		defer logFile.Close()
		all = io.MultiWriter(&combined, logFile)
	}
	out := all
	if opts.FailNonempty {
		out = io.MultiWriter(all, &stdoutOnly)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", step.Cmd)
	cmd.Stdout = out
	cmd.Stderr = all
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "FORCE_COLOR=0", "CLICOLOR=0", "CLICOLOR_FORCE=0")
	// The step gets its own process group so that stopping it reaches the
	// tools the shell started, not only the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = stopGrace

	start := time.Now()
	runErr := cmd.Start()
	if runErr == nil {
		stop := make(chan struct{})
		erased := make(chan struct{})
		go func() {
			showProgress(step.Label, start, stop)
			close(erased)
		}()
		runErr = cmd.Wait()
		close(stop)
		<-erased // the progress line is cleared before the summary prints
		if ctx.Err() != nil {
			// Anything in the group that ignored SIGTERM must not outlive a
			// run that has given up its slot.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // ESRCH when the group is already gone
		}
	}

	r := Result{
		Step:     step,
		Dur:      time.Since(start),
		Combined: stripANSI(combined.String()),
		LogPath:  logPath,
		OK:       true,
	}
	switch code, reason := outcome(ctx, cmd, runErr); {
	case code != 0 || reason != "":
		r.OK, r.ExitCode, r.Reason = false, code, reason
	case opts.FailNonempty && strings.TrimSpace(stripANSI(stdoutOnly.String())) != "":
		r.OK, r.ExitCode, r.Reason = false, 1, "produced output (expected none)"
	}
	return r
}

// outcome reduces how a step ended to an exit code and, when the code alone
// would mislead, a reason. Zero and no reason means it passed.
func outcome(ctx context.Context, cmd *exec.Cmd, runErr error) (code int, reason string) {
	if ctx.Err() != nil {
		return 130, "interrupted"
	}
	if cmd.ProcessState == nil {
		// Never started: sh missing, or the working directory is gone.
		return -1, runErr.Error()
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		// The shell usually reports a killed child as 128+signal itself, but
		// not when it exec'd the command in its own place.
		return 128 + int(ws.Signal()), ""
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		return code, ""
	}
	if runErr != nil && !errors.Is(runErr, exec.ErrWaitDelay) {
		return -1, runErr.Error()
	}
	// ErrWaitDelay with exit 0: the step finished and a process it left
	// behind still holds its output open. The step itself passed.
	return 0, ""
}

// sanitize makes a label safe to use as a file name.
func sanitize(label string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, label)
}
