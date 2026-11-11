<h1 align="center">qrun</h1>

<p align="center">A machine-wide queue and resource budget for heavy commands.</p>

<p align="center">
  <a href="https://github.com/sustinbebustin/qrun/actions/workflows/ci.yml"><img src="https://github.com/sustinbebustin/qrun/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
</p>

## What Is qrun?

A small Go CLI that sits in front of a project's build, lint and test commands, so that many checkouts, worktrees or coding agents verifying at once cannot overload the machine or flood an agent with output.

Each `go test`, `tsc`, linter and bundler sizes itself to the whole machine. Fifteen of them at once ask for fifteen machines: memory fills, the kernel swaps, and everything, your shell included, stalls. And every one of those runs prints pages of passing output that an agent reads into its context window for nothing.

- **Queue:** runs wait in one machine-wide line, first come, first served. By default one runs at a time.
- **Budget:** whatever is running shares a fixed ceiling, by default half the CPUs and half the RAM with swap off. The rest stays free for interactive work.
- **Summarise:** a run prints one line when it passes, and only the error lines plus a log path when it fails. An agent spends its tokens and its context window on the failure, not on the passing output around it.

qrun is not an agent orchestrator and needs no daemon, no agent configuration and no cooperation from the caller. It goes inside the project's own `verify` recipe, and every person and every agent that runs the recipe is throttled without knowing it exists.

## Prerequisites

- Linux or macOS, on amd64 or arm64.
- [systemd](https://systemd.io/) with a user manager -- for the budget only, so Linux only. Without it (macOS, a container with no systemd) the queue and the summaries work and the budget is skipped.

## Install

### Homebrew (macOS, Linux)

```sh
brew install --cask sustinbebustin/tap/qrun
```

### Pre-built binary

Download the archive for your OS and architecture from the [latest release](https://github.com/sustinbebustin/qrun/releases/latest), extract it, and put `qrun` somewhere on your `PATH`. Each release has binaries for darwin and linux on amd64 and arm64, plus a `checksums.txt`.

### Go install

Requires [Go](https://go.dev/) 1.21 or later; the newer toolchain the module needs is fetched automatically.

```sh
go install github.com/sustinbebustin/qrun/cmd/qrun@latest
```

Run `qrun status` to verify the install. It prints the limits in force and says so if this machine cannot enforce the budget.

## Contents

- [Quick start](#quick-start)
- [Wire it into a project](#wire-it-into-a-project)
- [Commands](#commands)
- [Scripting and agents](#scripting-and-agents)
- [Configuration](#configuration)
- [How it works](#how-it-works)
- [Troubleshooting](#troubleshooting)
- [Limitations](#limitations)
- [Development](#development)

## Quick start

1. Run your checks through qrun. Each `--step` is a label and a shell command; they run in order, and every step runs even if an earlier one fails, so one run reports everything.

```sh
qrun --title verify \
  --step 'vet=go vet ./...' \
  --step 'test=go test ./...'
```

```text
[PASS] verify  41.2s  ·  vet test
```

2. Break a test and run it again. The summary keeps the lines that say what failed and where, and points at the full log.

```text
[FAIL] verify  38.7s
  [PASS] vet   6.1s
  [FAIL] test  32.6s  (exit 1)
    --- FAIL: TestPrice (0.00s)
        price_test.go:7: expected 120, got 100
    FAIL
    FAIL	example.com/app/price	0.002s
    FAIL
    log: /tmp/qrun-1000/logs/app-377585c3/test.log
verify FAIL: test
```

3. Start the same command in a second checkout while the first is running. It waits its turn and says so on stderr.

```text
qrun: queued at position 1; running now: verify in ~/src/app (pid 41270, 12.4s). Cancelling loses this place in line.
qrun: started after 29.3s in line
[PASS] verify  40.8s  ·  vet test
```

4. See the line and the limits from anywhere.

```sh
qrun status
```

```text
running (1 of 1 slot):
  verify  in ~/src/app  pid 41270  for 1m12s
waiting (2), next first:
  1. verify  in ~/src/app-worktrees/fix-login  pid 41388  waited 58.0s
  2. go test  in ~/src/api  pid 41502  waited 9.1s
budget: 11 CPUs, memory capped at 12G with swap off, shared by all runs (qrun.slice)
settings: defaults (create ~/.config/qrun/config.toml to change them)
```

## Wire it into a project

qrun only throttles what goes through it, so it belongs in the recipe everyone already runs. Two forms, depending on who runs the recipe.

**Your own machines:** the quiet run. In a `justfile`:

```just
# Format, lint and test. Run it bare: the summary is the output.
verify:
    @qrun --title verify \
      --step 'lint=golangci-lint run ./...' \
      --step 'test=go test -race ./...'
```

**A shared or public repository,** where qrun may not be installed: the pass-through form behind an optional prefix. The command's own output and exit status are unchanged, and without qrun (a contributor's laptop, CI) the prefix is empty.

```just
# Optional: wait in the machine-wide line when qrun is installed.
gate := `command -v qrun >/dev/null 2>&1 && echo 'qrun exec --' || true`

verify:
    {{gate}} go vet ./...
    {{gate}} go test -race ./...
```

Both are ordinary command lines, so a Makefile or a `package.json` script takes them the same way.

## Commands

| Command | What it does |
|---|---|
| `qrun --step 'label=command' [--step ...]` | Quiet run. Waits for a slot, runs the steps in order inside the budget, prints a summary. `--title NAME` names a multi-step run. `--log-dir DIR` moves the `<label>.log` files (default: a per-checkout directory under `$TMPDIR`; a failure prints the exact path). `--show-on-success` also prints a passing step's output, for a step whose output is the point (a generated diff). `--fail-nonempty` fails a step that exits 0 but writes to stdout, for format checks such as `gofmt -l`. |
| `qrun exec -- command [args...]` | The same queue and budget in front of one command, with its stdin, stdout, stderr and exit status passed through. `--title NAME` sets the name shown to other runs (default: the command and its subcommand, such as `go test`). |
| `qrun status` | What is running, what is waiting in order, the limits in force and the settings file. `--json` for a machine-readable form. |
| `qrun version` | Print the version. |

`--no-queue`, on a quiet run or on `exec`, skips the line and still runs inside the budget. It is for short jobs, such as a lint of one package after an edit.

`qrun --help`, `qrun exec --help` and `qrun status --help` each list their flags with examples.

## Scripting and agents

qrun never prompts, and a usage error exits before anything runs or waits, with a correct example invocation.

- **stdout is the result, stderr is commentary.** The summary (or, under `exec`, the command's own output) is on stdout. Queue messages and progress go to stderr.
- **Exit status.** Quiet run: `0` every step passed, `1` a step failed, `2` usage error, `130` interrupted. `exec`: the command's own status, `127` if it cannot be started, `130` if interrupted while queued. `status`: `0`, or `1` if the queue cannot be read.
- **Progress.** On a terminal, a spinner that erases itself. On a pipe or in a background run, a `... <step> running (40.0s)` line every 20 seconds, and while queued, `qrun: still queued after 1m30s, position 2` every 30 seconds, so a waiting run is never mistaken for a hung one.
- **Read the summary, not the log.** A failed step's excerpt is its diagnostics: `file:line` references, test failures, panics and compiler errors across common toolchains, or the tail of the output when nothing matches. Color is switched off for the steps. The full log is always written, and its path is printed for the cases the excerpt does not settle.
- **Do not cancel a queued run to retry it.** A cancelled run loses its place and rejoins at the back. With many agents the wait can outlast a default command timeout, so give verification a long timeout or run it in the background.
- **A step may call qrun.** A run that already holds a slot does not queue behind itself.

An agent only gets the benefit if it runs the recipe bare. Piping the summary through `tail`, `grep` or a redirect buys nothing and can hide the failure. For a project's agent instructions:

```markdown
## Verification

Run `just verify` bare: no pipes, `grep`, `tail` or redirects. Its output is
already reduced to one line on success, or the failing lines and a log path.
Read the log only when the excerpt is not enough.

`qrun: queued at position N` means another run on this machine holds the slot.
Wait for it. Do not cancel and retry: a cancelled run goes to the back of the line.
```

## Configuration

One file for the whole machine, `~/.config/qrun/config.toml` (or under `$XDG_CONFIG_HOME`). Everything in it is optional. These are the defaults on a 22-core, 24 GB machine:

```toml
# Runs allowed at once, machine-wide. 0 turns the queue off.
slots = 1

[budget]
# false runs everything with no CPU or memory limit.
enabled = true
# CPU quota in whole cores. 0 means no CPU limit. Default: half the cores.
cpus = 11
# Hard memory cap, enforced with swap off. "none" removes it. Default: half of RAM.
# A whole number with K, M, G or T, or a percentage of RAM such as "50%".
memory_max = "12G"
# Soft limit that throttles before the cap. Default: none; see How it works.
memory_high = "none"
```

- The file is read by every run, so an edit takes effect on the next one. `qrun status` shows the values in force.
- Settings are per machine, never per caller: every run shares one line and one budget, so two callers with different numbers would each enforce a different limit on the same machine.
- An invalid file is reported with every problem in it, and the defaults are used. A run is never blocked by its own throttle.

Two environment variables opt a single run out without changing the shared limits:

| Variable | Effect |
|---|---|
| `QRUN_BYPASS=1` | Run now, with no queue and no budget. For a person who needs a result immediately. |
| `QRUN_NO_PROGRESS=1` | Silence the spinner and the `... running` lines. |

## How it works

**The queue** is files under one per-user directory (`/run/user/<uid>/qrun`, or `/tmp/qrun-<uid>`), and every claim on it is an `flock` held by the claiming process. A run takes the next ticket number, then waits until no live ticket has a lower number and a slot is free. Only the head of the line may take a slot, which is what makes the order strict. A run that crashes or is killed gives up its ticket and its slot the moment the kernel closes its files, so there is no daemon and nothing to clean up.

**The budget** is one systemd user slice, `qrun.slice`. qrun re-executes itself inside a transient scope under it, and the CPU quota and memory cap sit on the slice, so every run on the machine shares them however many overlap. The memory cap is hard, with swap off: when it is hit the kernel kills one process inside the slice, the step fails with exit 137 and a hint, and the rest of the machine never starts swapping. A soft limit (`memory_high`) is off by default because, below the cap, it throttles a run into continuous reclaim instead of letting it finish or fail.

**The summary** exists to save the reader's attention, and for an agent that means tokens. A passing build and test run can be thousands of lines that change nothing about what to do next, and an agent that reads them carries them in its context window for the rest of the session. qrun captures each step's output to a log file and prints only what decides the next action: that it passed, or which lines failed. Nothing is hidden, because the full log is always on disk.

## Troubleshooting

| Symptom | Check |
|---|---|
| A run waits and nothing seems to be running | `qrun status` names the run holding the slot, with its directory and pid. A stuck command there holds the line for everyone; stop it and the next run starts at once. |
| A step fails with `exit 137` | The kernel killed a process because the shared budget hit `memory_max`. `journalctl -k \| grep oom-kill` confirms. Rerun once other runs finish, lower the step's own parallelism, or raise `budget.memory_max`. |
| `qrun status` says the budget is not enforced | This machine has no systemd user manager with the cpu controller delegated (macOS, most containers). The queue still applies. |
| `settings file ignored` on stderr | The settings file has an unknown key or an invalid value. The message lists each one; the defaults are in force until it is fixed. |
| Runs from one tool wait but another's do not | Only commands that go through qrun are throttled. Check that the second tool runs the recipe and not the underlying command. |

## Limitations

- **The budget is Linux-only.** Elsewhere the queue and the summaries still work.
- **A run killed with SIGKILL leaves its children running.** Its slot is freed at once, so the next run can overlap with the orphans until they exit. They stay inside the budget. SIGINT, SIGTERM and SIGHUP are forwarded and stop the whole step.
- **Only commands that go through qrun are throttled.** A tool an agent runs directly is not.
- **Windows is not supported.**

## Development

```sh
make test            # unit tests, with the race detector
make e2e             # the built binary across real processes: order, crashes, signals
make lint
make release-check   # validate the release config and build every archive, publishing nothing
make ci              # everything CI runs
```

To release: move the `[Unreleased]` entries in [CHANGELOG.md](./CHANGELOG.md) under `## [X.Y.Z] - <date>`, push to `main`, and publish a GitHub release tagged `vX.Y.Z` with that section as its notes. Publishing runs [release.yml](./.github/workflows/release.yml): GoReleaser attaches the archives and checksums to the release and pushes the cask to [sustinbebustin/homebrew-tap](https://github.com/sustinbebustin/homebrew-tap). A prerelease tag such as `v1.2.0-rc.1` gets the archives and stays out of the tap.

Agent notes are in [CLAUDE.md](./CLAUDE.md).

## License

[MIT](./LICENSE)
