# A run that leaves the line keeps its place for a rerun

Coding agents run commands under a timeout that is often shorter than the wait when many of them verify at once. A cancelled run used to give its place up, so every retry rejoined at the back and a caller could be starved by its own retries. A run that leaves the line before it is admitted, cancelled or killed, now leaves a held place: for the length of `hold` (5 minutes by default) a rerun, the same command line from the same directory, takes that place back. A held place is not a run in line. Admission never waits for one, so the order among runs that are actually waiting is unchanged and a place nobody comes back for delays no one.

## Considered options

- **Keep the head of the line for the absent run.** Rejected: a run that never returns would stall every other run for the whole hold.
- **A daemon that owns the queue and outlives its callers.** Rejected: having no daemon is what makes crash recovery free. A dead process drops its locks and nothing has to notice.
- **Leave it to callers to use long timeouts.** Rejected: qrun's premise is that it needs no cooperation from the caller.

## Consequences

- The hold is counted from the ticket file's modification time, which a waiting run renews every few seconds, and not from a time written when the run leaves. A killed run cannot write anything, and its place must be held too.
- A ticket is probed with a shared lock. A held ticket is looked at by every waiting run on every poll for as long as the hold lasts, and with exclusive probes two of them looking at once would each take the other for the ticket's owner and report a position that flickers.
- A rerun is matched by directory and a digest of the whole command line, not by title: every untitled multi-step run is titled `run`, and a different command must not take the place. The digest stands in for the arguments because they can carry a secret and the ticket is shown by `qrun status --json`. Another invocation of the identical command in the same checkout can still take the place. That is accepted: it is the same work.
- A qrun built before held places removes any ticket it finds unlocked. Mixed versions on one machine therefore fall back to the old behaviour instead of breaking the order.
