package steps

import (
	"fmt"
	"os"
	"time"
)

// spinnerFrames is plain ASCII so it renders in any terminal.
var spinnerFrames = []string{"|", "/", "-", "\\"}

const (
	// spinnerInterval is how often the spinner repaints on a terminal.
	spinnerInterval = 120 * time.Millisecond
	// HeartbeatInterval is how often a stream that is not a terminal (an
	// agent's background run, a CI log) gets a "still running" line: long
	// enough to stay quiet on fast runs, frequent enough to prove liveness on
	// slow ones.
	HeartbeatInterval = 20 * time.Second
)

// showProgress reports that a step is running until stop is closed. On a
// terminal it animates an in-place spinner with the elapsed time and erases
// it when done, leaving the summary as the only output. Otherwise it prints a
// periodic heartbeat line. It writes to stderr only; stdout carries the
// summary. QRUN_NO_PROGRESS=1 turns it off.
func showProgress(label string, start time.Time, stop <-chan struct{}) {
	if os.Getenv("QRUN_NO_PROGRESS") != "" {
		<-stop
		return
	}
	w := os.Stderr
	tty := IsTerminal(w)

	interval := HeartbeatInterval
	if tty {
		interval = spinnerInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for frame := 0; ; frame++ {
		select {
		case <-stop:
			if tty {
				fmt.Fprint(w, "\r\x1b[K") // erase the spinner line
			}
			return
		case <-ticker.C:
			if tty {
				fmt.Fprintf(w, "\r\x1b[K  %s %s  %s", spinnerFrames[frame%len(spinnerFrames)], label, Dur(time.Since(start)))
			} else {
				fmt.Fprintf(w, "... %s running (%s)\n", label, Dur(time.Since(start)))
			}
		}
	}
}

// IsTerminal reports whether f is a character device, which is how a
// terminal shows up.
func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
