# qrun

Run heavy commands one at a time, inside a shared CPU and memory budget.

When several checkouts, worktrees or coding agents verify their work at the same moment, each `go test`, `tsc`, linter and bundler sizes itself to the whole machine. Fifteen of them at once ask for fifteen machines. Memory fills, the kernel swaps, and everything, including your shell, stalls.

qrun sits in front of those commands and does three things:

- **Queues.** Runs wait in one machine-wide line, first come, first served. By default one runs at a time.
- **Budgets.** Whatever is running shares a fixed ceiling: by default half the CPUs and half the RAM, with swap off. The rest stays free for interactive work.
- **Summarises.** A quiet run prints one line when it passes, and only the error lines plus a log path when it fails.

It needs no daemon and no cooperation from the caller. Put it inside the project's own `verify` recipe and every person and every agent is throttled without knowing it exists.

## Install

With Homebrew, on Linux or macOS:

```sh
brew install --cask sustinbebustin/tap/qrun
```

With Go:

```sh
go install github.com/sustinbebustin/qrun/cmd/qrun@latest
```

From a checkout: `make install`. Pre-built archives are attached to each [release](https://github.com/sustinbebustin/qrun/releases).

## Use

A quiet run, for a verification recipe:

```sh
qrun --title verify \
  --step 'lint=golangci-lint run' \
  --step 'test=go test -race ./...'
```

```text
[PASS] verify  41.2s  ·  lint test
```

```text
[FAIL] verify  38.7s
  [PASS] lint  6.1s
  [FAIL] test  32.6s  (exit 1)
    --- FAIL: TestPrice (0.00s)
        price_test.go:21: expected 120, got 100
    log: /tmp/qrun-1000/logs/app-3f9a1c2e/test.log
verify FAIL: test
```

Every step runs even if an earlier one fails, so one run reports everything. Run it bare: the summary is the output, and the full log is always on disk.

Pass-through, to put the throttle in front of an existing command line without changing its output or exit status:

```sh
qrun exec -- go test -race ./...
```

This is the form for a public repository, where qrun may not be installed. In a `justfile`:

```just
# Optional: wait in the machine-wide line when qrun is installed.
gate := `command -v qrun >/dev/null 2>&1 && echo 'qrun exec --' || true`

verify:
    {{gate}} go vet ./...
    {{gate}} go test -race ./...
```

See what is happening:

```sh
qrun status
```

```text
running (1 of 1 slot):
  verify  in ~/src/app  pid 41270  for 1m12s
waiting (2), next first:
  1. verify  in ~/src/app-worktrees/fix-login  pid 41388  waited 58.0s
  2. lint  in ~/src/api  pid 41502  waited 9.1s
budget: 11 CPUs, memory capped at 12G with swap off, shared by all runs (qrun.slice)
settings: defaults (create ~/.config/qrun/config.toml to change them)
```

`qrun --help`, `qrun exec --help` and `qrun status --help` list every flag with examples.

### Waiting

A run that has to wait says so on stderr, with its position and what is running, and repeats its position every 30 seconds. **Do not cancel a queued run to retry it:** a cancelled run loses its place and rejoins at the back. Give agents a command timeout long enough for the line, or have them run verification in the background.

`--no-queue` skips the line and still runs inside the budget. It is for short jobs, such as a lint of one package after an edit.

`QRUN_BYPASS=1` runs with no queue and no budget, for when a person needs a result now.

## Settings

One file, `~/.config/qrun/config.toml` (or under `$XDG_CONFIG_HOME`), for the whole machine. Everything is optional; these are the defaults on a 22-core, 24 GB machine:

```toml
# Runs allowed at once, machine-wide. 0 turns the queue off.
slots = 1

[budget]
enabled = true
# CPU quota in whole cores. 0 means no CPU limit. Default: half the cores.
cpus = 11
# Hard memory cap, with swap off. "none" removes it. Default: half of RAM.
# Sizes are a whole number with K, M, G or T, or a percentage of RAM.
memory_max = "12G"
# Soft limit that throttles before the cap. Default: none. See below.
memory_high = "none"
```

Settings are per machine, not per caller, on purpose: every run shares one line and one budget, so two callers with different numbers would each be enforcing a different limit on the same machine.

An invalid file is reported and the defaults are used. A run is never blocked by its own throttle.

## How it works

**The queue** is files under one per-user directory (`/run/user/<uid>/qrun`, or `/tmp/qrun-<uid>`), and every claim on it is an `flock` held by the claiming process. A run takes the next ticket number, then waits until no live ticket has a lower number and a slot is free. Only the head of the line may take a slot, which is what makes the order strict. A run that crashes or is killed gives up its ticket and its slot the moment the kernel closes its files, so nothing needs cleaning up.

**The budget** is one systemd user slice, `qrun.slice`. qrun re-executes itself inside a transient scope under it, and the CPU quota and memory cap sit on the slice, so every run on the machine shares them however many overlap. The memory cap is hard, with swap off: when it is hit the kernel kills one process inside the slice, the step fails with exit 137 and a hint, and the rest of the machine never starts swapping. A soft limit (`memory_high`) is off by default because, below the cap, it throttles a run into continuous reclaim instead of letting it finish or fail.

**Quiet output** keeps lines that look like diagnostics (`file:line`, test failures, panics, compiler errors) across common toolchains, with the tail of the output as a fallback, and always writes the full log.

## Limits

- **The budget is Linux-only.** It needs a systemd user manager with the cpu controller delegated. Elsewhere (macOS, containers without systemd) the queue and the quiet output still work, and `qrun status` says the budget is not enforced.
- **A run killed with SIGKILL leaves its children running.** Its slot is freed at once, so the next run can overlap with the orphans until they exit. They stay inside the budget. SIGINT, SIGTERM and SIGHUP are forwarded and stop the whole step.
- **Only commands that go through qrun are throttled.** A tool an agent runs directly is not.
- **Windows is not supported.**

## Develop

```sh
make test   # unit tests, with the race detector
make e2e    # the built binary across real processes: order, crashes, signals
make lint
make ci     # everything CI runs
```

## Release

1. Move the `[Unreleased]` entries in [CHANGELOG.md](./CHANGELOG.md) under `## [X.Y.Z] - <date>` and push to `main`.
2. Publish a GitHub release tagged `vX.Y.Z` with that section as its notes.

Publishing starts `.github/workflows/release.yml`: GoReleaser attaches the archives and checksums to the release and pushes the cask to [sustinbebustin/homebrew-tap](https://github.com/sustinbebustin/homebrew-tap). A prerelease tag (`v1.2.0-rc.1`) gets the archives but stays out of the tap. `make release-check` is the dry run.
