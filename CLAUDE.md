# qrun -- agent notes

A Go CLI that queues heavy commands machine-wide and runs them inside a shared CPU and memory budget. [README.md](./README.md) is the user-facing description; keep it in step with behaviour and with the `--help` texts in `cmd/qrun/main.go`.

It ships as a public binary: no project names, organisations or local paths in code, tests, examples or docs.

`make test` for unit tests, `make e2e` for the cross-process checks, `make ci` before pushing.

Record every user-visible change under `[Unreleased]` in [CHANGELOG.md](./CHANGELOG.md); it becomes the release notes. Releases follow the README's Release section. The CI job names are the required status checks of the repository's `main` ruleset, so renaming a job means updating the ruleset.

## Layout

- `cmd/qrun` -- argument parsing, help, and the wiring in `throttle.go`: queue, then budget, then the work.
- `internal/gate` -- the queue. Its package comment is the design.
- `internal/budget` -- the systemd slice. Its package comment is the rationale for each limit.
- `internal/steps` -- running labeled steps and filtering their output.
- `internal/config` -- the one machine-wide settings file: its keys, and the in-place editing behind `qrun config set`.

## Rules that are easy to break

- **Shared state is located by uid, never by environment.** The queue directory and the systemd socket must resolve to the same place for every process of a user. Sandboxes and agent harnesses rewrite `TMPDIR` and `XDG_RUNTIME_DIR` per session; two processes that disagreed would run two queues.
- **Queue and budget settings come from the settings file, never from flags or environment.** Per-caller values would mean each caller enforcing a different limit on the same machine. `--no-queue` and `QRUN_BYPASS` opt one run out; they do not retune the shared limits. `qrun config set` is an editor for that file, not a layer over it.
- **A setting is one entry in the `keys` table of `internal/config`.** Parsing, `qrun config`, its help and the generated file all read that table; the README's key table is the one copy kept by hand.
- **A run is never blocked by the throttle's own failure.** A bad settings file, an unusable queue directory or a missing systemd all warn on stderr and run anyway.
- **Lock files are never inherited by children.** A step's leftover daemon would otherwise hold a slot for as long as it lived.
- **stdout is the result, stderr is commentary.** Queue and progress messages go to stderr so `qrun exec` stays a faithful pass-through.
- A queue change needs a case in `scripts/e2e.sh`: in-process tests cannot see what a killed process leaves behind.

## Agent skills

### Issue tracker

GitHub Issues in this repo via `gh`. See `docs/agents/issue-tracker.md`.

### Triage labels

Default five roles, label = role name. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: root `GLOSSARY.md` + `docs/adr/`. See `docs/agents/domain.md`.
