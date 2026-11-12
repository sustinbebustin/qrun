#!/usr/bin/env bash
# End-to-end checks of queue order, crash recovery and signals, across real
# processes. Usage: e2e.sh /path/to/qrun
#
# It uses the real per-user queue, so it waits behind any run in progress.
# Nothing here depends on timing: a step that must still be running is held
# open by a flag file, and the script waits until `qrun status` shows a run
# before acting on it.
set -u
Q=$1
W=$(mktemp -d)
export QRUN_NO_PROGRESS=1
# Its own settings file: the checks assume the defaults, and one of them
# changes a setting.
export XDG_CONFIG_HOME="$W/config"
cd "$W" || exit 1
fail=0

check() {
  if [ "$2" = "$3" ]; then
    echo "  [PASS] $1"
  else
    echo "  [FAIL] $1: got '$2', want '$3'"
    fail=1
  fi
}

# hold NAME prints a shell command that blocks until `release NAME` is called.
hold() { echo "while [ ! -e $W/flag.$1 ]; do sleep 0.05; done"; }
release() { touch "$W/flag.$1"; }

# titles SECTION prints the titles `qrun status` lists under running, waiting
# or held.
titles() {
  "$Q" status --json | awk -v section="\"$1\"" '
    /"running"|"waiting"|"held"/ { on = index($0, section) > 0 }
    on && /"title"/ { gsub(/.*"title": "|",?$/, ""); print }'
}

# await SECTION TITLE waits up to 10s for a run to show under running, waiting,
# held or (for "listed") either of the first two.
await() {
  local section=$1 title=$2
  for _ in $(seq 1 200); do
    case $section in
      listed) { titles running; titles waiting; } | grep -qx "$title" && return 0 ;;
      *) titles "$section" | grep -qx "$title" && return 0 ;;
    esac
    sleep 0.05
  done
  echo "  [FAIL] timed out waiting for '$title' to be $section"
  fail=1
  return 1
}

# await_procs PATTERN COUNT waits up to 10s for exactly COUNT matching processes.
await_procs() {
  for _ in $(seq 1 200); do
    [ "$(pgrep -fc "$1")" = "$2" ] && return 0
    sleep 0.05
  done
  return 1
}

echo "== arrival order is admission order (5 runs, 1 slot)"
: > order
"$Q" --step "r1=echo 1 >> $W/order; $(hold order)" >out1 2>err1 &
await running r1
for n in 2 3 4 5; do
  "$Q" --step "r$n=echo $n >> $W/order" >"out$n" 2>"err$n" &
  await waiting "r$n"
done
check "one running, four waiting, in order" "$(titles running | tr '\n' ' ')| $(titles waiting | tr '\n' ' ')" "r1 | r2 r3 r4 r5 "
"$Q" status > status.mid
release order
wait
check "order" "$(tr '\n' ' ' < order)" "1 2 3 4 5 "
check "the last run was told its position" "$(grep -c 'queued at position 4; running now: r1 in' err5)" "1"
check "and each move up the line" "$(grep -c 'now at position' err5)" "3"
sed 's/^/    /' err5 status.mid

echo "== a killed waiter (SIGKILL) does not block the line"
: > order
"$Q" --step "a=echo a >> $W/order; $(hold killed)" >/dev/null 2>&1 &
await running a
"$Q" --step "dead=echo dead >> $W/order" >/dev/null 2>&1 & dead=$!
await waiting dead
"$Q" --step "c=echo c >> $W/order" >/dev/null 2>&1 &
await waiting c
kill -KILL $dead
wait $dead 2>/dev/null
release killed
wait
check "order" "$(tr '\n' ' ' < order)" "a c "

echo "== a cancelled waiter (SIGTERM) leaves the line and exits 130"
: > order
"$Q" --step "a=echo a >> $W/order; $(hold cancelled)" >/dev/null 2>&1 &
await running a
"$Q" --step "quit=echo quit >> $W/order" >/dev/null 2>err.quit & quit=$!
await waiting quit
kill -TERM $quit
wait $quit; check "exit" "$?" "130"
release cancelled
wait
check "order" "$(tr '\n' ' ' < order)" "a "
check "message" "$(grep -c 'interrupted while queued; nothing was run' err.quit)" "1"

echo "== a cancelled or killed waiter's place is held for its rerun"
: > order
"$Q" --step "a=echo a >> $W/order; $(hold held)" >/dev/null 2>&1 &
await running a
"$Q" --step "b=echo b >> $W/order" >/dev/null 2>err.b & b=$!
await waiting b
"$Q" --step "c=echo c >> $W/order" >/dev/null 2>&1 & c=$!
await waiting c
"$Q" --step "d=echo d >> $W/order" >/dev/null 2>&1 &
await waiting d
kill -TERM $b; wait $b
kill -KILL $c; wait $c 2>/dev/null
await held c
check "the cancelled run was told its place is held" "$(grep -c 'place in line is held for 5m' err.b)" "1"
check "only the run still in line is waiting" "$(titles waiting | tr '\n' ' ')" "d "
# The waiters the earlier checks stopped still have held places of their own.
check "both places are held" "$(titles held | grep -x -e b -e c | tr '\n' ' ')" "b c "
# Another command that happens to share b's title is not b's rerun.
"$Q" --step "b=echo e >> $W/order" >/dev/null 2>&1 &
for _ in $(seq 1 200); do [ "$(titles waiting | wc -l)" -ge 2 ] && break; sleep 0.05; done
check "a different command joins at the back" "$(titles waiting | tr '\n' ' ')" "d b "
# Rerun in the other order: the place decides, not the moment of the rerun.
"$Q" --step "c=echo c >> $W/order" >/dev/null 2>&1 &
await waiting c
"$Q" --step "b=echo b >> $W/order" >/dev/null 2>err.b2 &
for _ in $(seq 1 200); do [ "$(titles waiting | wc -l)" -ge 4 ] && break; sleep 0.05; done
check "the reruns are back ahead of the runs that stayed" "$(titles waiting | tr '\n' ' ')" "b c d b "
check "the rerun was told it took its place back" "$(grep -c 'queued at position 1, the place held from an earlier attempt' err.b2)" "1"
release held
wait
check "order" "$(tr '\n' ' ' < order)" "a b c d e "
check "neither place is still held" "$(titles held | grep -cx -e b -e c)" "0"

echo "== a new slot count reaches a run that is already waiting"
"$Q" --step "one=$(hold slots)" >/dev/null 2>&1 &
await running one
"$Q" --step "two=$(hold slots)" >/dev/null 2>&1 &
await waiting two
"$Q" config set slots 2 >/dev/null
check "status shows the new count" "$("$Q" status | head -1)" "running (1 of 2 slots):"
await running two
check "both run at once" "$(titles running | tr '\n' ' ')" "one two "
"$Q" config unset slots >/dev/null
release slots
wait
check "back to one slot" "$("$Q" status | head -1)" "running: none (1 slot free)"

echo "== SIGTERM to a running run stops its children and frees the slot"
# A duration no other process on the machine will be sleeping for.
nap="300.$$$RANDOM"
"$Q" --step "long=sh -c 'sleep $nap; touch $W/survived' & wait" >out.term 2>&1 & run=$!
await_procs "^sleep $nap" 1; check "the grandchild is running" "$?" "0"
kill -TERM $run
wait $run; check "exit" "$?" "130"
check "summary says interrupted" "$(grep -c 'interrupted' out.term)" "1"
await_procs "^sleep $nap" 0; check "no grandchild left behind" "$?" "0"
check "slot is free" "$("$Q" status | head -1)" "running: none (1 slot free)"

echo "== a run that outlasts the time limit is stopped and frees the slot"
"$Q" config set time_limit 1s >/dev/null
nap="300.$$$RANDOM"
"$Q" --step "stuck=sh -c 'sleep $nap; touch $W/outlived' & wait" --step "later=touch $W/outlived" >out.limit 2>err.limit
check "exit" "$?" "124"
check "the summary says why" "$(grep -c 'timed out at the 1s time limit' out.limit)" "1"
check "and that the later step did not run" "$(grep -c '1 later step(s) not run' out.limit)" "1"
check "stderr names the setting to change" "$(grep -c 'qrun config set time_limit' err.limit)" "1"
await_procs "^sleep $nap" 0; check "no grandchild left behind" "$?" "0"
check "nothing ran past the limit" "$([ -e "$W/outlived" ] && echo yes || echo no)" "no"
check "slot is free" "$("$Q" status | head -1)" "running: none (1 slot free)"
"$Q" exec -- sleep "$nap" 2>/dev/null
check "exec is stopped the same way" "$?" "124"
# The limit is on holding a slot, and this run holds none.
"$Q" --no-queue --step "free=sleep 2" >out.free 2>&1
check "a run outside the line has no limit" "$(grep -c '^\[PASS\] free' out.free)" "1"
"$Q" config unset time_limit >/dev/null

echo "== SIGKILL of a running run frees the slot"
# The killed run's step lives on for a while as an orphan. A lock inherited
# by it would keep the slot busy until it exited.
"$Q" --step "long=sleep 8" >/dev/null 2>&1 & run=$!
await running long
kill -KILL $run; wait $run 2>/dev/null
check "slot is free while the orphaned step still runs" "$(titles running | wc -l | tr -d ' ')" "0"
timeout 4 "$Q" --step "next=true" >/dev/null 2>&1
check "the next run did not wait for the orphan" "$?" "0"

echo "== --no-queue starts while the slot is busy"
"$Q" --step "busy=$(hold noqueue)" >/dev/null 2>&1 &
await running busy
"$Q" --no-queue --step "quick=true" > out.nq 2>err.nq
check "ran" "$(grep -c '^\[PASS\] quick' out.nq)" "1"
check "while the slot was still held" "$(titles running | tr '\n' ' ')" "busy "
check "no queue message" "$(wc -c < err.nq | tr -d ' ')" "0"
release noqueue
wait

echo "== a step that calls qrun does not wait on its own parent"
timeout 20 "$Q" --step "outer=$Q --step 'inner=echo nested' && $Q exec -- echo nested-exec" > out.nest 2>&1
check "exit" "$?" "0"

echo "== exec waits in the same line and passes the exit code through"
: > order
"$Q" --step "a=echo a >> $W/order; $(hold exec)" >/dev/null 2>&1 &
await running a
"$Q" exec --title second -- sh -c "echo b >> $W/order; exit 7" 2>err.exec & second=$!
await waiting second
release exec
wait $second; check "exit" "$?" "7"
wait
check "order" "$(tr '\n' ' ' < order)" "a b "
check "exec was told it queued" "$(grep -c 'queued at position 1; running now: a in' err.exec)" "1"

echo "== QRUN_BYPASS runs outside the queue and the budget"
"$Q" --step "busy=$(hold bypass)" >/dev/null 2>&1 &
await running busy
QRUN_BYPASS=1 "$Q" --show-on-success --step 'cg=cat /proc/self/cgroup' > out.bypass 2>&1
check "ran while the slot was held" "$(grep -c '^\[PASS\] cg' out.bypass)" "1"
check "not in qrun.slice" "$(grep -c 'qrun.slice' out.bypass)" "0"
release bypass
wait

echo "== two checkouts with the same directory name keep separate logs"
mkdir -p one/app two/app
(cd one/app && "$Q" --step 'x=echo from-one; false' > "$W/out.one" 2>&1)
(cd two/app && "$Q" --step 'x=echo from-two; false' > "$W/out.two" 2>&1)
log1=$(sed -n 's/.*log: //p' out.one); log2=$(sed -n 's/.*log: //p' out.two)
check "distinct log paths" "$([ -n "$log1" ] && [ "$log1" != "$log2" ] && echo yes)" "yes"
check "first log intact" "$(cat "$log1" 2>/dev/null)" "from-one"

rm -rf "$W"
if [ $fail = 0 ]; then echo "ALL PASSED"; else echo "SOME FAILED"; fi
exit $fail
