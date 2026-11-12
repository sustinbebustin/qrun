# qrun

A machine-wide queue and resource budget in front of heavy commands, so that many checkouts or coding agents verifying at once cannot overload the machine or flood a reader with output.

## Language

### Runs

**Run**:
One invocation of qrun that executes work under the throttle.
_Avoid_: Job, task, build

**Quiet run**:
A run of labeled steps that prints a summary instead of the steps' output.
_Avoid_: Verify run, silent run

**Pass-through run**:
A run of one command whose output and exit status reach the caller untouched.
_Avoid_: Exec run, wrapped run

**Step**:
One labeled shell command in a quiet run.
_Avoid_: Stage, phase

**Summary**:
What a quiet run prints: one line when every step passes, and for a failed step its excerpt and the path of its log.
_Avoid_: Report, digest

**Excerpt**:
The lines of a failed step's output that say what failed and where.
_Avoid_: Snippet, tail

**Rerun**:
The same command run again from the same directory, after an attempt that left the queue before it was admitted.
_Avoid_: Retry (the caller retries; what qrun sees is a rerun)

### The queue

**Queue**:
The single machine-wide, first-come-first-served order in which runs are admitted. Also "the line".
_Avoid_: Lock, mutex, scheduler

**Slot**:
Permission for one run to execute. The number of slots is how many runs execute at once.
_Avoid_: Worker, lane, concurrency

**Admitted**:
A run is admitted when it takes a slot and starts its work.
_Avoid_: Scheduled, dequeued

**Place**:
A run's position in the queue, fixed when it first joins.
_Avoid_: Ticket, turn

**Held place**:
The place of a run that left the queue before it was admitted, kept for its rerun. It is not a run in line: nothing waits for it.
_Avoid_: Reservation, ghost, tombstone

**Hold**:
How long a held place is kept.
_Avoid_: Grace period, timeout

**Time limit**:
The longest a run may hold a slot before it is stopped. It bounds how long one run can block the queue, not how long a command may take: a run that holds no slot has none.
_Avoid_: Timeout, deadline, max runtime

### Limits

**Budget**:
The CPU and memory ceiling shared by every run on the machine, however many overlap.
_Avoid_: Quota, cap (a cap is one limit inside the budget)

**Throttle**:
The queue and the budget together: everything qrun puts between a caller and its command.
_Avoid_: Limiter, governor

**Bypass**:
Running one command outside the throttle entirely, at the caller's request.
_Avoid_: Override, skip

**Settings**:
The machine-wide values that shape the queue and the budget. They belong to the machine, never to a caller or a run.
_Avoid_: Configuration, options, preferences

**Settings file**:
The one file that holds the settings. `qrun config` reads and edits it.
_Avoid_: Config file, rc file
