package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// execArgs is a parsed `qrun exec` command line.
type execArgs struct {
	command []string
	title   string
	noQueue bool
	help    bool
}

// cmdExec runs one command under the queue and budget with its stdio and
// exit status passed through.
func cmdExec(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	args, err := parseExecArgs(argv)
	if err != nil {
		fmt.Fprint(stderr, usageError(err.Error(), "qrun exec -- go test ./...", "qrun exec --help"))
		return exitUsage
	}
	if args.help {
		fmt.Fprint(stdout, helpExec)
		return exitOK
	}
	// Resolved before queueing: a typo should fail now, not after the wait.
	path, err := exec.LookPath(args.command[0])
	if err != nil {
		fmt.Fprintf(stderr, "qrun: cannot run %q: %v\n  Nothing was run and no place in line was taken.\n", args.command[0], err)
		return exitNotFound
	}

	return throttled(ctx, stderr, args.title, args.noQueue, append([]string{"exec"}, argv...), func(ctx context.Context) int {
		// The command stays in this process group and on this terminal, so a
		// Ctrl-C reaches it directly and it can read stdin.
		cmd := exec.CommandContext(ctx, path, args.command[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		code, started := supervise(cmd)
		if !started {
			fmt.Fprintf(stderr, "qrun: %s could not be started\n", path)
			return exitNotFound
		}
		return code
	})
}

// parseExecArgs splits qrun's own flags from the command. The command starts
// after "--", or at the first argument that is not a flag.
func parseExecArgs(argv []string) (execArgs, error) {
	var a execArgs
	i := 0
flags:
	for ; i < len(argv); i++ {
		switch argv[i] {
		case "--":
			i++
			break flags
		case "--no-queue":
			a.noQueue = true
		case "--title":
			if i+1 >= len(argv) {
				return a, errors.New("--title needs a value")
			}
			i++
			a.title = argv[i] //nolint:gosec // G602: i < len(argv) is checked just above
		case "--help", "-h":
			a.help = true
			return a, nil
		default:
			if strings.HasPrefix(argv[i], "-") {
				return a, fmt.Errorf("unknown flag %q before the command; put the command after --", argv[i])
			}
			break flags
		}
	}
	a.command = argv[i:]
	if len(a.command) == 0 {
		return a, errors.New("no command given to exec")
	}
	if a.title == "" {
		a.title = defaultTitle(a.command)
	}
	return a, nil
}

// subcommandRE matches a bare word: not a flag, a path or a package pattern.
var subcommandRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// defaultTitle names a command for status and wait messages: its base name,
// plus its subcommand when the next argument looks like one, since "go"
// alone would not tell a test run from a vet.
func defaultTitle(command []string) string {
	title := filepath.Base(command[0])
	if len(command) > 1 && subcommandRE.MatchString(command[1]) {
		title += " " + command[1]
	}
	return title
}
