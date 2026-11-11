package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/sustinbebustin/qrun/internal/budget"
	"github.com/sustinbebustin/qrun/internal/config"
	"github.com/sustinbebustin/qrun/internal/gate"
	"github.com/sustinbebustin/qrun/internal/steps"
)

// statusReport is everything `qrun status` shows; its JSON form is the
// --json output.
type statusReport struct {
	Slots   int          `json:"slots"`
	Running []gate.Entry `json:"running"`
	Waiting []gate.Entry `json:"waiting"`
	Budget  budgetReport `json:"budget"`
	Config  configReport `json:"config"`
}

type budgetReport struct {
	Enabled bool `json:"enabled"`
	// Enforced is false when the budget is enabled but this machine cannot
	// apply it (no systemd user manager, as on macOS).
	Enforced   bool   `json:"enforced"`
	CPUs       int    `json:"cpus"`
	MemoryMax  string `json:"memory_max"`
	MemoryHigh string `json:"memory_high"`
	Slice      string `json:"slice"`
}

type configReport struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	// Error is set when the file exists but was rejected, so the defaults
	// are in force.
	Error string `json:"error,omitempty"`
}

// cmdStatus prints what is running, what is waiting and the limits in force.
func cmdStatus(argv []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, arg := range argv {
		switch arg {
		case "--json":
			asJSON = true
		case "--help", "-h":
			fmt.Fprint(stdout, helpStatus)
			return exitOK
		default:
			fmt.Fprint(stderr, usageError(fmt.Sprintf("unknown argument %q to status", arg), "qrun status --json", "qrun status --help"))
			return exitUsage
		}
	}

	path := config.Path()
	cfg, cfgErr := config.Load(path, config.Default(config.ThisMachine()))
	report := statusReport{
		Slots:   cfg.Slots,
		Running: []gate.Entry{},
		Waiting: []gate.Entry{},
		Budget: budgetReport{
			Enabled:    cfg.Budget.Enabled,
			Enforced:   cfg.Budget.Enabled && budget.Available(),
			CPUs:       cfg.Budget.CPUs,
			MemoryMax:  cfg.Budget.MemoryMax,
			MemoryHigh: cfg.Budget.MemoryHigh,
			Slice:      budget.SliceName,
		},
		Config: configReport{Path: path, Exists: fileExists(path)},
	}
	if cfgErr != nil {
		report.Config.Error = cfgErr.Error()
	}

	g := gate.Gate{Dir: gate.DefaultDir(), Slots: cfg.Slots, Poll: gate.DefaultPoll}
	st, err := g.Status()
	if err != nil {
		fmt.Fprintf(stderr, "qrun: cannot read the queue: %v\n  Runs started now would not wait in line. Check that the directory exists and belongs to you.\n", err)
		return exitFailed
	}
	report.Running = append(report.Running, st.Running...)
	report.Waiting = append(report.Waiting, st.Waiting...)

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "qrun: cannot write status: %v\n", err)
			return exitFailed
		}
		return exitOK
	}
	renderStatus(stdout, report, time.Now())
	return exitOK
}

// renderStatus writes the human-readable status.
func renderStatus(w io.Writer, r statusReport, now time.Time) {
	switch {
	case r.Slots == 0:
		fmt.Fprintln(w, "queue: off (slots = 0); runs start immediately")
	case len(r.Running) == 0:
		fmt.Fprintf(w, "running: none (%s free)\n", plural(r.Slots, "slot"))
	default:
		fmt.Fprintf(w, "running (%d of %s):\n", len(r.Running), plural(r.Slots, "slot"))
		for _, e := range r.Running {
			fmt.Fprintf(w, "  %s\n", entryLine(e, now, "for"))
		}
	}
	if len(r.Waiting) > 0 {
		fmt.Fprintf(w, "waiting (%d), next first:\n", len(r.Waiting))
		for i, e := range r.Waiting {
			fmt.Fprintf(w, "  %d. %s\n", i+1, entryLine(e, now, "waited"))
		}
	}

	switch {
	case !r.Budget.Enabled:
		fmt.Fprintln(w, "budget: off (budget.enabled = false)")
	case !r.Budget.Enforced:
		fmt.Fprintln(w, "budget: not enforced on this machine (needs a systemd user manager with the cpu controller); only the queue applies")
	default:
		fmt.Fprintf(w, "budget: %s, %s, shared by all runs (%s)\n", cpuDesc(r.Budget.CPUs), memoryDesc(r.Budget), r.Budget.Slice)
	}

	switch {
	case r.Config.Error != "":
		fmt.Fprintf(w, "settings: invalid, so the defaults are in force:\n%s\n", indent(r.Config.Error))
	case r.Config.Exists:
		fmt.Fprintf(w, "settings: %s\n", r.Config.Path)
	default:
		fmt.Fprintf(w, "settings: defaults (create %s to change them)\n", r.Config.Path)
	}
}

func entryLine(e gate.Entry, now time.Time, verb string) string {
	return fmt.Sprintf("%s  in %s  pid %d  %s %s", e.Title, tildePath(e.Dir), e.PID, verb, steps.Dur(now.Sub(e.Since)))
}

func cpuDesc(cpus int) string {
	if cpus == 0 {
		return "no CPU limit"
	}
	return plural(cpus, "CPU")
}

func memoryDesc(b budgetReport) string {
	desc := "no memory cap"
	if b.MemoryMax != "" {
		desc = "memory capped at " + b.MemoryMax + " with swap off"
	}
	if b.MemoryHigh != "" {
		desc += ", throttled above " + b.MemoryHigh
	}
	return desc
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
