#!/usr/bin/env bash
# End-to-end checks of queue order, crash recovery and signals, across real
# processes. Usage: e2e.sh /path/to/qrun
set -u
Q=$1
W=$(mktemp -d)
export QRUN_NO_PROGRESS=1
cd "$W" || exit 1
fail=0
check() { if [ "$2" = "$3" ]; then echo "  [PASS] $1"; else echo "  [FAIL] $1: got '$2', want '$3'"; fail=1; fi; }

echo "== arrival order is admission order (5 runs, 1 slot)"
: > order
for n in 1 2 3 4 5; do
  "$Q" --step "r$n=echo $n >> $W/order; sleep 0.4" >"out$n" 2>"err$n" &
  sleep 0.15
done
sleep 0.2
"$Q" status > status.mid
wait
check "order" "$(tr '\n' ' ' < order)" "1 2 3 4 5 "
check "last run was told its position" "$(grep -c 'queued at position' err5)" "1"
check "status showed a run and a line" "$(grep -c -E '^(running \(1 of 1 slot\)|waiting \()' status.mid)" "2"
sed 's/^/    /' err5 status.mid

echo "== a killed waiter (SIGKILL) does not block the line"
: > order
"$Q" --step "a=echo a >> $W/order; sleep 1" >/dev/null 2>&1 &
sleep 0.2
"$Q" --step "dead=echo dead >> $W/order" >/dev/null 2>&1 & dead=$!
sleep 0.2
"$Q" --step "c=echo c >> $W/order" >/dev/null 2>&1 &
sleep 0.2
kill -KILL $dead
wait
check "order" "$(tr '\n' ' ' < order)" "a c "

echo "== a cancelled waiter (SIGTERM) leaves the line and exits 130"
: > order
"$Q" --step "a=echo a >> $W/order; sleep 1" >/dev/null 2>&1 &
sleep 0.2
"$Q" --step "quit=echo quit >> $W/order" >/dev/null 2>err.quit & quit=$!
sleep 0.3
kill -TERM $quit
wait $quit; check "exit" "$?" "130"
wait
check "order" "$(tr '\n' ' ' < order)" "a "
check "message" "$(grep -c 'interrupted while queued; nothing was run' err.quit)" "1"

echo "== SIGTERM to a running run stops its children and frees the slot"
# A duration no other process on the machine will be sleeping for.
nap="300.$$$RANDOM"
"$Q" --step "long=sh -c 'sleep $nap; touch $W/survived' & wait" >out.term 2>&1 & run=$!
sleep 0.5
check "the grandchild is running" "$(pgrep -fc "^sleep $nap")" "1"
kill -TERM $run
wait $run; check "exit" "$?" "130"
check "summary says interrupted" "$(grep -c 'interrupted' out.term)" "1"
sleep 0.3
check "no grandchild left behind" "$(pgrep -fc "^sleep $nap")" "0"
check "slot is free" "$("$Q" status | head -1)" "running: none (1 slot free)"

echo "== SIGKILL of a running run frees the slot"
"$Q" --step "long=sleep 3" >/dev/null 2>&1 & run=$!
sleep 0.4
kill -KILL $run; wait $run 2>/dev/null
start=$(date +%s%N)
"$Q" --step "next=true" >/dev/null 2>&1
check "next run did not wait for the dead one" "$(( ($(date +%s%N) - start) / 1000000 < 1500 ))" "1"

echo "== --no-queue starts while the slot is busy"
"$Q" --step "busy=sleep 1.5" >/dev/null 2>&1 &
sleep 0.3
start=$(date +%s%N)
"$Q" --no-queue --step "quick=true" > out.nq 2>err.nq
check "did not wait" "$(( ($(date +%s%N) - start) / 1000000 < 1000 ))" "1"
check "no queue message" "$(wc -c < err.nq | tr -d ' ')" "0"
wait

echo "== a step that calls qrun does not wait on its own parent"
timeout 10 "$Q" --step "outer=$Q --step 'inner=echo nested' && $Q exec -- echo nested-exec" > out.nest 2>&1
check "exit" "$?" "0"

echo "== exec waits in the same line and passes the exit code through"
: > order
"$Q" --step "a=echo a >> $W/order; sleep 0.8" >/dev/null 2>&1 &
sleep 0.2
"$Q" exec --title second -- sh -c "echo b >> $W/order; exit 7" 2>err.exec
check "exit" "$?" "7"
wait
check "order" "$(tr '\n' ' ' < order)" "a b "
check "exec was told it queued" "$(grep -c 'queued at position 1; running now: a in' err.exec)" "1"

echo "== QRUN_BYPASS runs outside the queue and the budget"
"$Q" --step "busy=sleep 1" >/dev/null 2>&1 &
sleep 0.3
QRUN_BYPASS=1 "$Q" --show-on-success --step 'cg=cat /proc/self/cgroup' > out.bypass 2>&1
check "not in qrun.slice" "$(grep -c 'qrun.slice' out.bypass)" "0"
wait

echo "== two checkouts with the same directory name keep separate logs"
mkdir -p one/app two/app
(cd one/app && "$Q" --step 'x=echo from-one; false' > "$W/out.one" 2>&1)
(cd two/app && "$Q" --step 'x=echo from-two; false' > "$W/out.two" 2>&1)
log1=$(sed -n 's/.*log: //p' out.one); log2=$(sed -n 's/.*log: //p' out.two)
check "distinct log paths" "$([ -n "$log1" ] && [ "$log1" != "$log2" ] && echo yes)" "yes"
check "first log intact" "$(cat "$log1" 2>/dev/null)" "from-one"

rm -rf "$W"
[ $fail = 0 ] && echo "ALL PASSED" || echo "SOME FAILED"
exit $fail
