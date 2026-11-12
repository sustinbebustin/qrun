# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- A machine-wide, first-come-first-served queue for heavy commands. A run that crashes or is killed gives up its slot at once, with no daemon to clean up after it.
- A CPU and memory budget shared by every run, enforced through one systemd user slice (Linux): a CPU quota and a hard memory cap with swap off.
- `qrun --step 'label=command'`: a quiet run that prints one line when every step passes, and only the error lines plus a log path when one fails.
- `qrun exec -- command`: the same queue and budget in front of any command, with its output and exit status passed through unchanged.
- `qrun status [--json]`: what is running, what is waiting and the limits in force.
- One settings file for the whole machine, `~/.config/qrun/config.toml`, for the number of slots and the budget.
- `--no-queue` to skip the line for a short job while staying inside the budget, and `QRUN_BYPASS=1` to run with neither.
- `qrun config`: read and change the settings without an editor, with `show`, `get`, `set`, `unset`, `init`, `check` and `path`. `set` and `unset` keep the file's comments and take `--dry-run`.
- A held place in line: a run cancelled or killed while queued keeps its place for 5 minutes (the `hold` setting), and the same command run again from the same directory rejoins there instead of at the back.
- A change to `slots` reaches runs that are already waiting, within a couple of seconds.
- `qrun status` lists the held places and says why the budget is not enforced when it is not.
- A waiting run's periodic `still queued` line names the run holding the slot and how long it has held it.
