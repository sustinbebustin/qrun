// Command qrun runs heavy commands one at a time, inside a shared CPU and
// memory budget, so that many checkouts or coding agents verifying at once
// cannot overload the machine.
//
// See `qrun --help`, and the internal packages for how each part works:
// gate (the queue), budget (the limits) and steps (the quiet output).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

// Exit codes. A quiet run exits 0 or 1; `qrun exec` exits with its command's
// own code.
const (
	exitOK          = 0
	exitFailed      = 1
	exitUsage       = 2
	exitNotFound    = 127
	exitInterrupted = 130
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run dispatches to a subcommand. A first argument that is not a subcommand
// name starts a quiet run, so `qrun --step ...` needs no verb.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageError("no command given", "qrun --step 'test=go test ./...'", "qrun --help"))
		return exitUsage
	}
	switch args[0] {
	case "exec":
		return cmdExec(ctx, args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, helpRun)
		return exitOK
	case "version", "--version":
		fmt.Fprintf(stdout, "qrun %s\n", version)
		return exitOK
	default:
		return cmdRun(ctx, args, stdout, stderr)
	}
}

// usageError formats a usage failure: what was wrong, a correct invocation
// to copy, and the command that prints the relevant help.
func usageError(problem, example, help string) string {
	return fmt.Sprintf("qrun: %s\n  example: %s\n  help:    %s\n", problem, example, help)
}

const helpRun = `qrun - run heavy commands one at a time, inside a shared CPU and memory budget

Usage:
  qrun [flags] --step 'label=command' [--step ...]   quiet run: a summary, not the output
  qrun exec [flags] -- command [args...]             same throttle, output untouched
  qrun status [--json]                               what is running and what is waiting

A run first waits for a machine-wide slot, first come first served, then
executes inside a budget shared by every qrun on the machine. A quiet run
prints one line when every step passes; on failure it prints only the error
lines and the path of the full log. Run it bare: no pipes, grep or redirects.

Flags (quiet run):
  --step 'label=command'  shell command to run; repeat to run several in order.
                          Every step runs even if an earlier one fails.
  --title NAME            name for a multi-step run (default: the single step's label)
  --log-dir DIR           where <label>.log files go (default: a per-checkout directory
                          under $TMPDIR; a failure prints the exact path)
  --show-on-success       also print a passing step's output (a generated diff, a report)
  --fail-nonempty         fail a step that exits 0 but prints to stdout (gofmt -l)
  --no-queue              skip the line; still inside the budget. For short jobs only.

Examples:
  qrun --step 'test=go test ./...'
  qrun --title verify --step 'lint=golangci-lint run' --step 'test=go test -race ./...'
  qrun --fail-nonempty --step 'fmt=gofmt -l .'
  qrun --show-on-success --step 'diff=git diff --stat'

Exit status: 0 every step passed, 1 a step failed, 2 usage error, 130 interrupted.

While queued, qrun reports its position on stderr. Do not cancel a queued run
to retry it: a cancelled run loses its place and rejoins at the back.

Settings are machine-wide, in the file ` + "`qrun status`" + ` names. QRUN_BYPASS=1 runs
with no queue and no budget. QRUN_NO_PROGRESS=1 silences the progress lines.

More: qrun exec --help, qrun status --help
`

const helpExec = `qrun exec - run one command under the queue and budget, output untouched

Usage:
  qrun exec [flags] -- command [args...]

The command's stdin, stdout, stderr and exit status pass straight through, so
it can stand in front of any existing command line. Use it where the caller
wants the tool's own output; use ` + "`qrun --step`" + ` for a summary instead.

Flags:
  --title NAME   name shown in ` + "`qrun status`" + ` and to runs waiting behind this one
                 (default: the command's name and subcommand, such as "go test")
  --no-queue     skip the line; still inside the budget. For short jobs only.

Examples:
  qrun exec -- go test -race ./...
  qrun exec --title lint -- golangci-lint run ./...
  qrun exec --no-queue -- gofmt -l .

Exit status: the command's own; 127 if it cannot be started, 130 if interrupted
while queued.
`

const helpStatus = `qrun status - show what is running, what is waiting, and the limits in force

Usage:
  qrun status [--json]

Flags:
  --json   machine-readable output

Examples:
  qrun status
  qrun status --json

Exit status: 0, or 1 if the queue cannot be read.
`
