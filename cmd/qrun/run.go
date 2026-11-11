package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sustinbebustin/qrun/internal/steps"
)

// runArgs is a parsed quiet-run command line.
type runArgs struct {
	steps   []steps.Step
	opts    steps.Options
	noQueue bool
	help    bool
}

// cmdRun is the quiet run: `qrun [flags] --step 'label=command' ...`.
func cmdRun(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	// Arguments are validated before anything else, so a usage error never
	// waits in line or pays for a re-exec.
	args, err := parseRunArgs(argv)
	if err != nil {
		fmt.Fprint(stderr, usageError(err.Error(), "qrun --title verify --step 'lint=golangci-lint run' --step 'test=go test ./...'", "qrun --help"))
		return exitUsage
	}
	if args.help {
		fmt.Fprint(stdout, helpRun)
		return exitOK
	}
	if err := os.MkdirAll(args.opts.LogDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "qrun: cannot create the log directory %s: %v\n  Nothing was run. Pass a writable directory with --log-dir.\n", args.opts.LogDir, err)
		return exitUsage
	}

	return throttled(ctx, stderr, args.opts.Title, args.noQueue, argv, func(ctx context.Context) int {
		start := time.Now()
		results := steps.Run(ctx, args.steps, args.opts)
		steps.Render(stdout, args.opts, results, len(args.steps), time.Since(start))
		switch {
		case ctx.Err() != nil:
			return exitInterrupted
		case !steps.AllOK(results, len(args.steps)):
			return exitFailed
		}
		return exitOK
	})
}

// parseRunArgs parses a quiet run's flags. Each --step is "label=command":
// the label is everything before the first '=', the command the rest, since
// commands routinely contain '=' themselves.
func parseRunArgs(argv []string) (runArgs, error) {
	var a runArgs
	for i := 0; i < len(argv); i++ {
		flag := argv[i]
		value := func() (string, error) {
			if i+1 >= len(argv) {
				return "", fmt.Errorf("%s needs a value", flag)
			}
			i++
			return argv[i], nil
		}
		var err error
		switch flag {
		case "--step":
			var v string
			if v, err = value(); err == nil {
				label, cmd, ok := strings.Cut(v, "=")
				if !ok || strings.TrimSpace(label) == "" || strings.TrimSpace(cmd) == "" {
					return a, fmt.Errorf("--step must be 'label=command', got %q", v)
				}
				a.steps = append(a.steps, steps.Step{Label: strings.TrimSpace(label), Cmd: cmd})
			}
		case "--title":
			a.opts.Title, err = value()
		case "--log-dir":
			a.opts.LogDir, err = value()
		case "--show-on-success":
			a.opts.ShowOnSuccess = true
		case "--fail-nonempty":
			a.opts.FailNonempty = true
		case "--no-queue":
			a.noQueue = true
		case "--help", "-h":
			a.help = true
			return a, nil
		default:
			return a, fmt.Errorf("unknown argument %q", flag)
		}
		if err != nil {
			return a, err
		}
	}
	if len(a.steps) == 0 {
		return a, errors.New("no --step given")
	}
	if a.opts.Title == "" {
		if len(a.steps) == 1 {
			a.opts.Title = a.steps[0].Label
		} else {
			a.opts.Title = "run"
		}
	}
	if a.opts.LogDir == "" {
		a.opts.LogDir = defaultLogDir()
	}
	return a, nil
}

// defaultLogDir is ${TMPDIR:-/tmp}/qrun-<uid>/logs/<directory name>-<hash>.
// The hash is of the full working directory: worktrees of one repository
// usually share a directory name, and a log named after that alone would be
// overwritten by the next checkout's run before anyone read it. The uid keeps
// users on a shared machine out of each other's way.
func defaultLogDir() string {
	name := "unknown"
	if wd, err := os.Getwd(); err == nil {
		sum := sha256.Sum256([]byte(wd))
		name = fmt.Sprintf("%s-%x", filepath.Base(wd), sum[:4])
	}
	return filepath.Join(os.TempDir(), "qrun-"+strconv.Itoa(os.Getuid()), "logs", name)
}
