# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- A machine-wide, first-come-first-served queue for heavy commands. A run that crashes or is killed gives up its place and its slot at once, with no daemon to clean up after it.
- A CPU and memory budget shared by every run, enforced through one systemd user slice (Linux): a CPU quota and a hard memory cap with swap off.
- `qrun --step 'label=command'`: a quiet run that prints one line when every step passes, and only the error lines plus a log path when one fails.
- `qrun exec -- command`: the same queue and budget in front of any command, with its output and exit status passed through unchanged.
- `qrun status [--json]`: what is running, what is waiting and the limits in force.
- One settings file for the whole machine, `~/.config/qrun/config.toml`, for the number of slots and the budget.
- `--no-queue` to skip the line for a short job while staying inside the budget, and `QRUN_BYPASS=1` to run with neither.
