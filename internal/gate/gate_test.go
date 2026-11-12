package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

const testPoll = 2 * time.Millisecond

func newGate(t *testing.T, slots int) Gate {
	t.Helper()
	return Gate{Dir: t.TempDir(), Slots: slots, Poll: testPoll}
}

func who(title string) Entry {
	return Entry{Title: title, Dir: "/work/" + title, PID: os.Getpid(), Since: time.Unix(1_700_000_000, 0).UTC()}
}

func titles(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Title
	}
	return out
}

// waiter starts an Acquire in the background and reports its milestones.
type waiter struct {
	queued   chan Wait   // every onWait call
	acquired chan func() // the release func, once admitted
	failed   chan error
}

func startWaiter(ctx context.Context, g Gate, title string) waiter {
	w := waiter{queued: make(chan Wait, 16), acquired: make(chan func(), 1), failed: make(chan error, 1)}
	go func() {
		rel, err := g.Acquire(ctx, who(title), func(wt Wait) { w.queued <- wt })
		if err != nil {
			w.failed <- err
			return
		}
		w.acquired <- rel
	}()
	return w
}

func mustQueue(t *testing.T, w waiter) Wait {
	t.Helper()
	select {
	case wt := <-w.queued:
		return wt
	case <-w.acquired:
		t.Fatal("run was admitted when it should have queued")
	case err := <-w.failed:
		t.Fatalf("Acquire failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("run neither queued nor was admitted")
	}
	return Wait{}
}

func mustAcquire(t *testing.T, w waiter) func() {
	t.Helper()
	select {
	case rel := <-w.acquired:
		return rel
	case err := <-w.failed:
		t.Fatalf("Acquire failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("run was not admitted")
	}
	return nil
}

func mustStayQueued(t *testing.T, w waiter) {
	t.Helper()
	select {
	case <-w.acquired:
		t.Fatal("run was admitted out of turn")
	case err := <-w.failed:
		t.Fatalf("Acquire failed: %v", err)
	case <-time.After(20 * testPoll):
	}
}

func TestAdmitsUpToSlotsWithoutQueueing(t *testing.T) {
	g := newGate(t, 2)
	var releases []func()
	for _, title := range []string{"a", "b"} {
		rel, err := g.Acquire(context.Background(), who(title), func(Wait) { t.Errorf("%s queued with a free slot", title) })
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, rel)
	}
	for _, rel := range releases {
		rel()
	}
}

func TestQueuesBeyondSlotsAndReportsWhoIsRunning(t *testing.T) {
	g := newGate(t, 1)
	releaseA, err := g.Acquire(context.Background(), who("a"), nil)
	if err != nil {
		t.Fatal(err)
	}

	b := startWaiter(context.Background(), g, "b")
	wt := mustQueue(t, b)
	if wt.Position != 1 {
		t.Errorf("position = %d, want 1 (next in line)", wt.Position)
	}
	if len(wt.Running) != 1 || wt.Running[0].Title != "a" || wt.Running[0].Dir != "/work/a" || wt.Running[0].PID != os.Getpid() {
		t.Errorf("running = %+v, want the run holding the slot", wt.Running)
	}
	if since := wt.Running[0].Since; time.Since(since) > time.Minute {
		t.Errorf("running since %v, want the moment it took the slot", since)
	}

	releaseA()
	mustAcquire(t, b)()
}

func TestAdmitsInArrivalOrder(t *testing.T) {
	g := newGate(t, 1)
	release, err := g.Acquire(context.Background(), who("first"), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Each waiter is queued before the next one starts, so arrival order is
	// unambiguous.
	titles := []string{"w1", "w2", "w3", "w4", "w5"}
	waiters := make([]waiter, len(titles))
	for i, title := range titles {
		waiters[i] = startWaiter(context.Background(), g, title)
		if wt := mustQueue(t, waiters[i]); wt.Position != i+1 {
			t.Errorf("%s queued at position %d, want %d", title, wt.Position, i+1)
		}
	}

	for i, w := range waiters {
		release()
		release = mustAcquire(t, w)
		for _, later := range waiters[i+1:] {
			mustStayQueued(t, later)
		}
	}
	release()
}

func TestReportsPositionAsTheLineMoves(t *testing.T) {
	g := newGate(t, 1)
	release, err := g.Acquire(context.Background(), who("running"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ahead := startWaiter(context.Background(), g, "ahead")
	mustQueue(t, ahead)
	behind := startWaiter(context.Background(), g, "behind")
	if wt := mustQueue(t, behind); wt.Position != 2 {
		t.Fatalf("position = %d, want 2", wt.Position)
	}

	release()
	releaseAhead := mustAcquire(t, ahead)
	if wt := mustQueue(t, behind); wt.Position != 1 {
		t.Errorf("position after the line moved = %d, want 1", wt.Position)
	}
	releaseAhead()
	mustAcquire(t, behind)()
}

func TestAWaiterThatGivesUpLeavesTheLine(t *testing.T) {
	g := newGate(t, 1)
	release, err := g.Acquire(context.Background(), who("running"), nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	quitter := startWaiter(ctx, g, "quitter")
	mustQueue(t, quitter)
	patient := startWaiter(context.Background(), g, "patient")
	mustQueue(t, patient)

	cancel()
	select {
	case err := <-quitter.failed:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}

	release()
	mustAcquire(t, patient)()
}

func TestADeadWaiterDoesNotBlockTheLine(t *testing.T) {
	g := newGate(t, 1)
	release, err := g.Acquire(context.Background(), who("running"), nil)
	if err != nil {
		t.Fatal(err)
	}

	// A ticket nobody holds a lock on is what a killed waiter leaves behind.
	stale := filepath.Join(g.Dir, queueDir, ticketName(1))
	if err := os.WriteFile(stale, []byte(`{"title":"killed"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	w := startWaiter(context.Background(), g, "alive")
	if wt := mustQueue(t, w); wt.Position != 1 {
		t.Errorf("position = %d, want 1: the dead waiter must not count", wt.Position)
	}
	release()
	mustAcquire(t, w)()

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale ticket still present (stat error: %v)", err)
	}
}

func TestReleaseFreesTheSlotAndIsIdempotent(t *testing.T) {
	g := newGate(t, 1)
	release, err := g.Acquire(context.Background(), who("a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()

	rel, err := g.Acquire(context.Background(), who("b"), func(Wait) { t.Error("queued behind a released slot") })
	if err != nil {
		t.Fatal(err)
	}
	rel()
}

func TestStatusListsRunningAndWaitingInOrder(t *testing.T) {
	g := newGate(t, 1)

	st, err := g.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Running) != 0 || len(st.Waiting) != 0 {
		t.Errorf("idle status = %+v, want empty", st)
	}

	release, err := g.Acquire(context.Background(), who("running"), nil)
	if err != nil {
		t.Fatal(err)
	}
	w1 := startWaiter(context.Background(), g, "w1")
	mustQueue(t, w1)
	w2 := startWaiter(context.Background(), g, "w2")
	mustQueue(t, w2)

	st, err = g.Status()
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(st.Running); !slices.Equal(got, []string{"running"}) {
		t.Errorf("running = %v", got)
	}
	// A waiting entry keeps the time it joined the line.
	if !slices.Equal(st.Waiting, []Entry{who("w1"), who("w2")}) {
		t.Errorf("waiting = %+v, want w1 then w2", st.Waiting)
	}

	release()
	mustAcquire(t, w1)()
	mustAcquire(t, w2)()
}

func TestClosesTheDirectoryToOtherUsers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // the unsafe mode is the case under test
		t.Fatal(err)
	}
	g := Gate{Dir: dir, Slots: 1, Poll: testPoll}
	rel, err := g.Acquire(context.Background(), who("a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	rel()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %04o, want 0700: another user could hold a slot or reap a ticket", perm)
	}
}

func TestRefusesARootThatIsNotADirectory(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	g := Gate{Dir: link, Slots: 1, Poll: testPoll}
	if _, err := g.Acquire(context.Background(), who("a"), nil); err == nil {
		t.Error("Acquire followed a symlink as its root; in /tmp another user could point it anywhere")
	}
}

// holding returns a gate that keeps a cancelled waiter's place for hold.
func holding(t *testing.T, hold time.Duration) Gate {
	t.Helper()
	g := newGate(t, 1)
	g.Hold = hold
	return g
}

// cancelWaiter cancels a queued waiter and waits for it to leave.
func cancelWaiter(t *testing.T, cancel context.CancelFunc, w waiter) {
	t.Helper()
	cancel()
	select {
	case err := <-w.failed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}
}

func TestACancelledWaiterKeepsItsPlaceForARerun(t *testing.T) {
	g := holding(t, time.Minute)
	release, err := g.Acquire(context.Background(), who("running"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := startWaiter(ctx, g, "retried")
	mustQueue(t, first)
	behind := startWaiter(context.Background(), g, "behind")
	if wt := mustQueue(t, behind); wt.Position != 2 {
		t.Fatalf("position = %d, want 2", wt.Position)
	}

	cancelWaiter(t, cancel, first)
	if wt := mustQueue(t, behind); wt.Position != 1 {
		t.Errorf("position with the run ahead gone = %d, want 1: a held place is not a run in line", wt.Position)
	}
	st, err := g.Status()
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(st.Waiting); !slices.Equal(got, []string{"behind"}) {
		t.Errorf("waiting = %v, want only the run still in line", got)
	}
	if got := titles(st.Held); !slices.Equal(got, []string{"retried"}) {
		t.Errorf("held = %v, want the cancelled run's place", got)
	}

	again := startWaiter(context.Background(), g, "retried")
	if wt := mustQueue(t, again); wt.Position != 1 || !wt.Rejoined {
		t.Errorf("rerun queued as %+v, want its old place at position 1, marked as rejoined", wt)
	}
	if wt := mustQueue(t, behind); wt.Position != 2 || wt.Rejoined {
		t.Errorf("the run behind was told %+v, want position 2", wt)
	}

	release()
	releaseAgain := mustAcquire(t, again)
	mustStayQueued(t, behind)
	releaseAgain()
	mustAcquire(t, behind)()
}

func TestAHeldPlaceDoesNotBlockTheLine(t *testing.T) {
	g := holding(t, time.Minute)
	release, err := g.Acquire(context.Background(), who("running"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	quitter := startWaiter(ctx, g, "quitter")
	mustQueue(t, quitter)
	patient := startWaiter(context.Background(), g, "patient")
	mustQueue(t, patient)

	cancelWaiter(t, cancel, quitter)
	release()
	mustAcquire(t, patient)()
}

func TestAHeldPlaceIsGivenUpWhenTheHoldRunsOut(t *testing.T) {
	g := holding(t, 40*time.Millisecond)
	release, err := g.Acquire(context.Background(), who("running"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := startWaiter(ctx, g, "late")
	mustQueue(t, first)
	other := startWaiter(context.Background(), g, "other")
	mustQueue(t, other)
	cancelWaiter(t, cancel, first)

	time.Sleep(2 * g.Hold)
	again := startWaiter(context.Background(), g, "late")
	if wt := mustQueue(t, again); wt.Position != 2 || wt.Rejoined {
		t.Errorf("rerun after the hold queued as %+v, want the back of the line", wt)
	}
	if st, err := g.Status(); err != nil || len(st.Held) != 0 {
		t.Errorf("held = %+v (error %v), want none once the hold ran out", st.Held, err)
	}

	release()
	mustAcquire(t, other)()
	mustAcquire(t, again)()
}

func TestAHeldPlaceIsOnlyForTheSameCommandInTheSameDirectory(t *testing.T) {
	for name, rerun := range map[string]Entry{
		"another title":     {Title: "other", Dir: "/work/held", PID: os.Getpid()},
		"another directory": {Title: "held", Dir: "/work/elsewhere", PID: os.Getpid()},
		// Two commands can share a title: every untitled multi-step run does.
		"another command line": {Title: "held", Dir: "/work/held", PID: os.Getpid(), Fingerprint: "other"},
	} {
		t.Run(name, func(t *testing.T) {
			g := holding(t, time.Minute)
			release, err := g.Acquire(context.Background(), who("running"), nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			first := startWaiter(ctx, g, "held")
			mustQueue(t, first)
			other := startWaiter(context.Background(), g, "other-run")
			mustQueue(t, other)
			cancelWaiter(t, cancel, first)

			queued := make(chan Wait, 16)
			admitted := make(chan func(), 1)
			go func() {
				rel, err := g.Acquire(context.Background(), rerun, func(wt Wait) { queued <- wt })
				if err == nil {
					admitted <- rel
				}
			}()
			select {
			case wt := <-queued:
				if wt.Position != 2 || wt.Rejoined {
					t.Errorf("queued as %+v, want the back of the line", wt)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("run did not queue")
			}
			release()
			mustAcquire(t, other)()
			(<-admitted)()
		})
	}
}

// A waiter that is killed cannot say it is leaving: all it leaves is a ticket
// with no lock on it.
func TestAKilledWaitersPlaceIsHeldToo(t *testing.T) {
	g := holding(t, time.Minute)
	release, err := g.Acquire(context.Background(), who("running"), nil)
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(g.Dir, queueDir, ticketName(1))
	if err := os.WriteFile(orphan, []byte(`{"title":"killed","dir":"/work/killed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	other := startWaiter(context.Background(), g, "other")
	if wt := mustQueue(t, other); wt.Position != 1 {
		t.Errorf("position = %d, want 1: the dead waiter must not count", wt.Position)
	}

	again := startWaiter(context.Background(), g, "killed")
	if wt := mustQueue(t, again); wt.Position != 1 || !wt.Rejoined {
		t.Errorf("rerun queued as %+v, want the killed run's place", wt)
	}
	release()
	releaseAgain := mustAcquire(t, again)
	mustStayQueued(t, other)
	releaseAgain()
	mustAcquire(t, other)()
}

func TestASlotCountChangeReachesAWaitingRun(t *testing.T) {
	var slots atomic.Int64
	slots.Store(1)
	g := newGate(t, 1)
	g.Reload = func() int { return int(slots.Load()) }

	releaseA, err := g.Acquire(context.Background(), who("a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := startWaiter(context.Background(), g, "b")
	mustQueue(t, b)
	c := startWaiter(context.Background(), g, "c")
	mustQueue(t, c)

	slots.Store(2)
	releaseB := mustAcquire(t, b)
	mustStayQueued(t, c)

	// 0 switches the queue off: nothing is left to wait for.
	slots.Store(0)
	mustAcquire(t, c)()
	releaseB()
	releaseA()
}
