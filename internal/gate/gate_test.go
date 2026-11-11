package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
