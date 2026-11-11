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
//	queue/<n>    one ticket per waiting run, flock'd by that run
//	slots/<i>    one file per slot, flock'd by the run executing in it
//
// A run takes the next ticket number, then waits until no live ticket has a
// lower number and a slot is free. Only the head of the line may take a slot,
// which is what makes the order strict: a later arrival cannot win a slot by
// happening to look at the right moment.
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
)

// Entry identifies a run in the queue or in a slot.
type Entry struct {
	Title string    `json:"title"`
	Dir   string    `json:"dir"`
	PID   int       `json:"pid"`
	Since time.Time `json:"since"` // when it joined the line, or took its slot
}

// Wait is the line as seen by a run that has to wait.
type Wait struct {
	Position int     // 1 is next to be admitted
	Running  []Entry // the runs holding the slots
}

// Status is a snapshot of the whole gate.
type Status struct {
	Running []Entry `json:"running"`
	Waiting []Entry `json:"waiting"` // in admission order
}

// Gate is a queue for Slots concurrent runs, rooted at Dir.
type Gate struct {
	Dir   string
	Slots int
	Poll  time.Duration
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
// a func that frees its slot. onWait, if set, is called when the run first
// has to wait and again each time its position changes. Cancelling ctx leaves
// the line and returns ctx's error.
//
// An error other than ctx's means the gate itself is unusable and no slot is
// held.
func (g Gate) Acquire(ctx context.Context, who Entry, onWait func(Wait)) (release func(), err error) {
	if err := g.prepare(); err != nil {
		return nil, err
	}
	t, err := g.enqueue(who)
	if err != nil {
		return nil, err
	}
	// The ticket goes only after the slot is held, so the next in line never
	// sees an empty line with the slot still free.
	defer t.leave()

	lastPosition := 0
	for {
		ahead, err := g.liveAhead(t.seq)
		if err != nil {
			return nil, err
		}
		if ahead == 0 {
			release, err := g.takeSlot(who)
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
				onWait(Wait{Position: position, Running: g.running()})
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(g.Poll):
		}
	}
}

// Status reports the runs holding slots and the runs waiting, in order.
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
		if live(path) {
			st.Waiting = append(st.Waiting, readEntry(path))
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

// enqueue allocates the next ticket number and publishes a locked ticket for
// it. Numbers are handed out under the seq lock, and a ticket becomes visible
// only by a rename made while that lock is held and after its own lock is
// taken, so tickets appear in the queue in number order and never unlocked.
func (g Gate) enqueue(who Entry) (*ticket, error) {
	seqPath := filepath.Join(g.Dir, seqFile)
	seqF, err := os.OpenFile(seqPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", seqPath, err)
	}
	defer seqF.Close() // closing drops the seq lock
	if err := flock(seqF, syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("lock %s: %w", seqPath, err)
	}

	next, err := g.nextSeq(seqF)
	if err != nil {
		return nil, err
	}

	queue := filepath.Join(g.Dir, queueDir)
	f, err := os.CreateTemp(queue, ".new-*") // dot-prefixed: not a ticket until renamed
	if err != nil {
		return nil, fmt.Errorf("create ticket in %s: %w", queue, err)
	}
	t := &ticket{seq: next, path: g.ticketPath(next), file: f}
	if err := publish(f, t.path, who); err != nil {
		_ = os.Remove(f.Name()) //nolint:errcheck // already failing; the temp file is not a ticket
		f.Close()
		return nil, err
	}
	if err := writeAll(seqF, strconv.FormatUint(next, 10)); err != nil {
		t.leave()
		return nil, fmt.Errorf("record ticket number in %s: %w", seqPath, err)
	}
	return t, nil
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
		if live(g.ticketPath(other)) {
			ahead++
		}
	}
	return ahead, nil
}

// live reports whether the ticket at path still has an owner. A ticket whose
// lock can be taken belonged to a run that died without leaving the line; it
// is removed so the line moves on.
func live(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false // already gone
	}
	defer f.Close()
	if err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Held by its owner. Any other failure is treated the same way: wrongly
		// keeping a ticket costs a wait, wrongly reaping one breaks the order.
		return true
	}
	_ = os.Remove(path) //nolint:errcheck // best effort: another prober may have reaped it first
	return false
}

// takeSlot claims a free slot, returning nil when all are busy.
func (g Gate) takeSlot(who Entry) (release func(), err error) {
	who.Since = time.Now().UTC()
	for i := range g.Slots {
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
