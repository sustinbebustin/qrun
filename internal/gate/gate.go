// Package gate is the machine-wide admission queue: a first-come,
// first-served line for a fixed number of slots, shared by every process that
// uses the same directory.
//
// A resource budget bounds how much CPU and memory heavy runs may use, but
// not how many are resident at once. N overlapping runs each hold their full
// working set while the budget stretches every one of them N times longer, so
// memory sums until the cap is hit and the kernel thrashes or kills a step.
// Running them one after another finishes each in its solo time, and the batch
// sooner, because nothing thrashes.
//
// There is no daemon. All state is files under one directory, and every claim
// on it is an flock held by the claiming process, so a run that crashes or is
// killed gives up its place and its slot the moment the kernel closes its
// files:
//
//	seq          counter for the next ticket number; flock'd while allocating
//	queue/<n>    one ticket per place in line, flock'd by the run waiting in it
//	slots/<i>    one file per slot, flock'd by the run executing in it
//
// A run takes the next ticket number, then waits until no live ticket has a
// lower number and a slot is free. Only the head of the line may take a slot,
// which is what makes the order strict: a later arrival cannot win a slot by
// happening to look at the right moment.
//
// A run that leaves the line before it is admitted, cancelled or killed,
// leaves its ticket behind with no lock on it. That is a held place: it is
// not a run in line, so nothing waits for it, but for the length of the hold
// a rerun of the same command line from the same directory takes the ticket
// back and with it the number. A caller with a short timeout, as coding agents
// have, would otherwise be sent to the back of the line by every retry and
// never be admitted. The hold is counted from the ticket's modification
// time, which its run keeps current while it waits, so a killed run's place
// is held as a cancelled one's is. See docs/adr/0001-held-places.md.
package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	seqFile  = "seq"
	queueDir = "queue"
	slotsDir = "slots"

	// DefaultPoll is how often a waiting run rechecks the line. Runs take
	// seconds to minutes, so the delay it adds to a handover is noise.
	DefaultPoll = 100 * time.Millisecond

	// touchEvery is how often a waiting run renews its ticket's modification
	// time. A killed run's hold is counted from its last renewal, so this is
	// how much of the hold a kill can cost.
	touchEvery = 5 * time.Second

	// A rerun claiming a held ticket can collide with a waiting run that is
	// looking at it. That look lasts microseconds, so a few short retries
	// tell it apart from a ticket that has an owner.
	claimTries = 5
	claimRetry = time.Millisecond
)

// Entry identifies a run in the queue or in a slot.
type Entry struct {
	Title string    `json:"title"`
	Dir   string    `json:"dir"`
	PID   int       `json:"pid"`
	Since time.Time `json:"since"` // when it joined the line, or took its slot
	// Fingerprint tells this command from another with the same title in
	// the same directory, so that only its own rerun takes its held place.
	// It is opaque: a digest of the command line, never the line itself.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Wait is the line as seen by a run that has to wait.
type Wait struct {
	Position int     // 1 is next to be admitted
	Running  []Entry // the runs holding the slots
	// Rejoined is true when this run took back the place held from an
	// earlier attempt instead of joining at the back.
	Rejoined bool
}

// Status is a snapshot of the whole gate.
type Status struct {
	Running []Entry `json:"running"`
	Waiting []Entry `json:"waiting"` // in admission order
	// Held is the places kept for runs that left the line, in line order.
	Held []Entry `json:"held"`
}

// Gate is a queue for Slots concurrent runs, rooted at Dir.
type Gate struct {
	Dir   string
	Slots int
	Poll  time.Duration
	// Hold is how long the place of a run that left the line before it was
	// admitted is kept for its rerun. 0 gives the place up at once.
	Hold time.Duration
	// Reload, when set, is asked for the slot count each time a waiting run
	// rechecks the line, in place of Slots, so that a change to the settings
	// reaches runs that are already waiting. 0 means the queue is now off.
	Reload func() int
}

// DefaultDir is the gate directory every qrun process of this user shares.
// It is derived from the uid alone, never from TMPDIR or XDG_RUNTIME_DIR:
// sandboxes and agent harnesses rewrite those per session, and two processes
// that resolved different directories would each run their own queue.
func DefaultDir() string {
	uid := strconv.Itoa(os.Getuid())
	if runtimeDir := "/run/user/" + uid; ownedDir(runtimeDir) == nil {
		return filepath.Join(runtimeDir, "qrun")
	}
	return "/tmp/qrun-" + uid
}

// Acquire joins the line and blocks until this run is admitted, then returns
// a func that frees its slot. A run whose earlier attempt left a held place
// rejoins there. onWait, if set, is called when the run first has to wait and
// again each time its position changes. Cancelling ctx leaves the line, keeps
// the place for Hold, and returns ctx's error.
//
// An error other than ctx's means the gate itself is unusable and no slot is
// held.
func (g Gate) Acquire(ctx context.Context, who Entry, onWait func(Wait)) (release func(), err error) {
	if err := g.prepare(); err != nil {
		return nil, err
	}
	t, rejoined, err := g.enqueue(who)
	if err != nil {
		return nil, err
	}
	release, err = g.admit(ctx, t, who, rejoined, onWait)
	if err != nil && ctx.Err() != nil && g.Hold > 0 {
		t.hold()
	} else {
		// On admission the ticket goes only now, after the slot is held, so
		// the next in line never sees an empty line with the slot still free.
		t.leave()
	}
	return release, err
}

// admit waits until t is at the head of the line and a slot is free, and
// takes the slot.
func (g Gate) admit(ctx context.Context, t *ticket, who Entry, rejoined bool, onWait func(Wait)) (release func(), err error) {
	lastPosition := 0
	touched := time.Now()
	for {
		slots := g.Slots
		if g.Reload != nil {
			slots = g.Reload()
		}
		if slots == 0 {
			return func() {}, nil
		}
		ahead, err := g.liveAhead(t.seq)
		if err != nil {
			return nil, err
		}
		if ahead == 0 {
			release, err := g.takeSlot(who, slots)
			if err != nil {
				return nil, err
			}
			if release != nil {
				return release, nil
			}
		}
		if position := ahead + 1; position != lastPosition {
			lastPosition = position
			if onWait != nil {
				onWait(Wait{Position: position, Running: g.running(), Rejoined: rejoined})
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(g.Poll):
		}
		if g.Hold > 0 && time.Since(touched) >= touchEvery {
			t.touch()
			touched = time.Now()
		}
	}
}

// Status reports the runs holding slots, the runs waiting and the held
// places, each in order.
func (g Gate) Status() (Status, error) {
	if err := g.prepare(); err != nil {
		return Status{}, err
	}
	st := Status{Running: g.running()}
	seqs, err := g.tickets()
	if err != nil {
		return Status{}, err
	}
	for _, seq := range seqs {
		path := g.ticketPath(seq)
		switch g.probe(path) {
		case placeLive:
			st.Waiting = append(st.Waiting, readEntry(path))
		case placeHeld:
			st.Held = append(st.Held, readEntry(path))
		case placeGone:
		}
	}
	return st, nil
}

// prepare creates the gate directories. The fallback root lives in /tmp,
// where another user could have created the name first, so the root must be
// a real directory this user owns, and it is closed to everyone else: anyone
// able to write there could hold a slot forever or reap a live ticket.
func (g Gate) prepare() error {
	if err := os.MkdirAll(g.Dir, 0o700); err != nil {
		return fmt.Errorf("create queue directory %s: %w", g.Dir, err)
	}
	if err := ownedDir(g.Dir); err != nil {
		return fmt.Errorf("queue directory %s %w", g.Dir, err)
	}
	if err := os.Chmod(g.Dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its owner's execute bit
		return fmt.Errorf("restrict queue directory %s to its owner: %w", g.Dir, err)
	}
	for _, dir := range []string{filepath.Join(g.Dir, queueDir), filepath.Join(g.Dir, slotsDir)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create queue directory %s: %w", dir, err)
		}
	}
	return nil
}

// ownedDir reports whether path is a real directory (not a symlink to one)
// owned by this user.
func ownedDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("is not a directory")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("is owned by uid %d, not by this user (uid %d)", st.Uid, os.Getuid())
	}
	return nil
}

// ticket is this run's place in the line.
type ticket struct {
	seq  uint64
	path string
	file *os.File // holds the flock that marks the ticket live
}

// leave removes the ticket from the line.
func (t *ticket) leave() {
	// Unlink before unlocking: a prober that finds the file unlocked would
	// otherwise treat a departing run as a dead one, which is harmless but
	// makes it do the removal.
	_ = os.Remove(t.path) //nolint:errcheck // best effort: a leftover unlocked ticket is reaped by the next prober
	t.file.Close()
}

// hold gives up the ticket's lock and leaves the ticket in the queue as a
// held place.
func (t *ticket) hold() {
	t.touch() // the hold runs from now, not from the last renewal
	t.file.Close()
}

// touch records that the ticket's run is alive now.
func (t *ticket) touch() {
	now := time.Now()
	_ = os.Chtimes(t.path, now, now) //nolint:errcheck // best effort: a stale time only shortens the hold
}

// enqueue gives who its place in line: the one held from its earlier attempt
// if there is one, else a new ticket with the next number. Numbers are
// handed out under the seq lock, and a ticket becomes visible only by a
// rename made while that lock is held and after its own lock is taken, so
// tickets appear in the queue in number order and never unlocked.
func (g Gate) enqueue(who Entry) (t *ticket, rejoined bool, err error) {
	seqPath := filepath.Join(g.Dir, seqFile)
	seqF, err := os.OpenFile(seqPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %w", seqPath, err)
	}
	defer seqF.Close() // closing drops the seq lock
	if err := flock(seqF, syscall.LOCK_EX); err != nil {
		return nil, false, fmt.Errorf("lock %s: %w", seqPath, err)
	}

	// Under the seq lock, so two reruns cannot take the same held place.
	if t := g.rejoin(who); t != nil {
		return t, true, nil
	}

	next, err := g.nextSeq(seqF)
	if err != nil {
		return nil, false, err
	}

	queue := filepath.Join(g.Dir, queueDir)
	f, err := os.CreateTemp(queue, ".new-*") // dot-prefixed: not a ticket until renamed
	if err != nil {
		return nil, false, fmt.Errorf("create ticket in %s: %w", queue, err)
	}
	t = &ticket{seq: next, path: g.ticketPath(next), file: f}
	if err := publish(f, t.path, who); err != nil {
		_ = os.Remove(f.Name()) //nolint:errcheck // already failing; the temp file is not a ticket
		f.Close()
		return nil, false, err
	}
	if err := writeAll(seqF, strconv.FormatUint(next, 10)); err != nil {
		t.leave()
		return nil, false, fmt.Errorf("record ticket number in %s: %w", seqPath, err)
	}
	return t, false, nil
}

// rejoin takes back the first held place left by a run of who's command in
// who's directory, or returns nil when there is none.
func (g Gate) rejoin(who Entry) *ticket {
	if g.Hold <= 0 {
		return nil
	}
	seqs, err := g.tickets()
	if err != nil {
		return nil
	}
	for _, seq := range seqs {
		path := g.ticketPath(seq)
		// Read before locking, so only a ticket that could be who's is locked.
		was := readEntry(path)
		if was.Title != who.Title || was.Dir != who.Dir || was.Fingerprint != who.Fingerprint {
			continue
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		// The time it first joined the line stays, so the wait shown for it
		// counts every attempt.
		was.PID = who.PID
		if g.claim(f, path) && writeEntry(f, was) == nil {
			return &ticket{seq: seq, path: path, file: f}
		}
		f.Close()
	}
	return nil
}

// claim locks the held ticket open as f for a new owner. It reports false
// when the ticket has an owner, its hold has run out, or it was reaped
// before the lock was taken.
func (g Gate) claim(f *os.File, path string) bool {
	// An owner's lock is the only thing a shared lock cannot join, so this
	// skips a ticket that has a run waiting in it without the retries below.
	if flock(f, syscall.LOCK_SH|syscall.LOCK_NB) != nil {
		return false
	}
	locked := false
	for range claimTries {
		if flock(f, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			locked = true
			break
		}
		time.Sleep(claimRetry)
	}
	if !locked {
		return false
	}
	// The lock may be on a file a prober has already unlinked.
	info, err := f.Stat()
	if err != nil {
		return false
	}
	current, err := os.Stat(path)
	return err == nil && os.SameFile(info, current) && g.inHold(info)
}

// inHold reports whether a ticket last renewed at info's modification time
// is still within the hold.
func (g Gate) inHold(info fs.FileInfo) bool {
	return g.Hold > 0 && time.Since(info.ModTime()) < g.Hold
}

// publish locks the new ticket file, fills it in and renames it into place.
func publish(f *os.File, path string, who Entry) error {
	if err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock new ticket %s: %w", f.Name(), err)
	}
	if err := writeEntry(f, who); err != nil {
		return fmt.Errorf("write ticket %s: %w", f.Name(), err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("publish ticket: %w", err)
	}
	return nil
}

// nextSeq returns the number after the highest one handed out. The counter
// file is the record, but the queue is consulted too, so a counter lost to a
// crash or a cleared file can never reissue a number that is still in line.
func (g Gate) nextSeq(seqF *os.File) (uint64, error) {
	var last uint64
	if data, err := os.ReadFile(seqF.Name()); err == nil {
		// An empty or corrupt counter parses as 0; the queue scan below still
		// keeps numbers moving forward.
		last, _ = strconv.ParseUint(string(data), 10, 64) //nolint:errcheck // 0 is the intended fallback
	}
	seqs, err := g.tickets()
	if err != nil {
		return 0, err
	}
	if len(seqs) > 0 {
		last = max(last, seqs[len(seqs)-1])
	}
	return last + 1, nil
}

// tickets returns the ticket numbers in the queue, ascending. Some may be
// stale; live decides.
func (g Gate) tickets() ([]uint64, error) {
	queue := filepath.Join(g.Dir, queueDir)
	entries, err := os.ReadDir(queue)
	if err != nil {
		return nil, fmt.Errorf("read queue %s: %w", queue, err)
	}
	var seqs []uint64
	for _, e := range entries {
		if seq, err := strconv.ParseUint(e.Name(), 10, 64); err == nil {
			seqs = append(seqs, seq)
		}
	}
	slices.Sort(seqs)
	return seqs, nil
}

// liveAhead counts the waiting runs whose tickets precede seq.
func (g Gate) liveAhead(seq uint64) (int, error) {
	seqs, err := g.tickets()
	if err != nil {
		return 0, err
	}
	ahead := 0
	for _, other := range seqs {
		if other >= seq {
			break
		}
		if g.probe(g.ticketPath(other)) == placeLive {
			ahead++
		}
	}
	return ahead, nil
}

// place is the state of a ticket in the queue.
type place int

const (
	// placeGone: no ticket, or one that nobody owns and no hold keeps.
	placeGone place = iota
	// placeLive: a run is waiting in it.
	placeLive
	// placeHeld: its run left the line and may still rejoin.
	placeHeld
)

// probe reports the state of the ticket at path. A ticket with no owner and
// no hold left is removed, so the queue does not fill with dead tickets.
func (g Gate) probe(path string) place {
	f, err := os.Open(path)
	if err != nil {
		return placeGone
	}
	defer f.Close() // releases the probe lock if it was taken
	// A shared lock, because a held place is probed by every waiting run on
	// every poll for as long as the hold lasts: with an exclusive lock, two
	// of them looking at once would each take the other for the owner.
	if err := flock(f, syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		// Locked by its owner. Any other failure is treated the same way: wrongly
		// keeping a ticket costs a wait, wrongly reaping one breaks the order.
		return placeLive
	}
	if info, err := f.Stat(); err == nil && g.inHold(info) {
		return placeHeld
	}
	_ = os.Remove(path) //nolint:errcheck // best effort: another prober may have reaped it first
	return placeGone
}

// takeSlot claims one of the first slots slots that is free, returning nil
// when all are busy.
func (g Gate) takeSlot(who Entry, slots int) (release func(), err error) {
	who.Since = time.Now().UTC()
	for i := range slots {
		path := filepath.Join(g.Dir, slotsDir, strconv.Itoa(i))
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open slot %s: %w", path, err)
		}
		if flock(f, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			f.Close()
			continue
		}
		// Naming the holder is for status and wait messages only; the lock is
		// what holds the slot, so a failed write does not give it up.
		_ = writeEntry(f, who) //nolint:errcheck // see above
		var once sync.Once
		return func() {
			once.Do(func() {
				_ = f.Truncate(0) //nolint:errcheck // best effort: a free slot's contents are never read
				f.Close()         // drops the lock
			})
		}, nil
	}
	return nil, nil
}

// running returns the runs that hold slots. It looks at every slot file, not
// only the first Slots, so runs admitted under a larger setting still show.
func (g Gate) running() []Entry {
	dir := filepath.Join(g.Dir, slotsDir)
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var entries []Entry
	for _, name := range names {
		path := filepath.Join(dir, name.Name())
		if busy(path) {
			entries = append(entries, readEntry(path))
		}
	}
	return entries
}

// busy reports whether a run holds the slot at path.
func busy(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close() // releases the probe lock if it was taken
	return flock(f, syscall.LOCK_EX|syscall.LOCK_NB) != nil
}

func (g Gate) ticketPath(seq uint64) string {
	return filepath.Join(g.Dir, queueDir, ticketName(seq))
}

// ticketName zero-pads so directory listings sort in admission order.
func ticketName(seq uint64) string {
	return fmt.Sprintf("%020d", seq)
}

// flock applies an flock operation, retrying when a signal interrupts it.
func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func writeEntry(f *os.File, e Entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return writeAll(f, string(data))
}

// writeAll replaces the contents of f.
func writeAll(f *os.File, s string) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.WriteAt([]byte(s), 0)
	return err
}

// readEntry reads who holds a ticket or slot. The file can be mid-rewrite or
// already gone, so an unreadable one yields a placeholder, not an error.
func readEntry(path string) Entry {
	data, err := os.ReadFile(path)
	var e Entry
	if err != nil || json.Unmarshal(data, &e) != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Entry{Title: "(finished)"}
		}
		return Entry{Title: "(unknown)"}
	}
	return e
}
