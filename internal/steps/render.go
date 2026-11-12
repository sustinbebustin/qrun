package steps

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Render writes the summary of a run. A single-step run collapses to one line
// on success; a multi-step run collapses to one line when everything passes.
// Failures print only the salient excerpt and a log pointer. want is the
// number of steps asked for, which exceeds len(results) when the run was
// interrupted; elapsed is the wall-clock time of the whole run.
func Render(w io.Writer, opts Options, results []Result, want int, elapsed time.Duration) {
	if want == 1 && len(results) == 1 && opts.Title == results[0].Step.Label {
		renderSingle(w, opts, results[0])
		return
	}
	renderMulti(w, opts, results, want, elapsed)
}

func renderSingle(w io.Writer, opts Options, r Result) {
	if r.OK {
		if opts.ShowOnSuccess && strings.TrimSpace(r.Combined) != "" {
			fmt.Fprint(w, ensureTrailingNL(r.Combined))
		}
		fmt.Fprintf(w, "[PASS] %s  %s\n", r.Step.Label, Dur(r.Dur))
		return
	}
	fmt.Fprintf(w, "[FAIL] %s  %s  (%s)\n", r.Step.Label, Dur(r.Dur), exitDesc(r))
	writeExcerpt(w, r, maxLinesSingle, "  ")
}

func renderMulti(w io.Writer, opts Options, results []Result, want int, elapsed time.Duration) {
	title := opts.Title
	if title == "" {
		title = "run"
	}
	var failed []string
	for _, r := range results {
		if !r.OK {
			failed = append(failed, r.Step.Label)
		}
	}
	skipped := want - len(results)

	if len(failed) == 0 && skipped == 0 {
		labels := make([]string, len(results))
		for i, r := range results {
			labels[i] = r.Step.Label
		}
		fmt.Fprintf(w, "[PASS] %s  %s  ·  %s\n", title, Dur(elapsed), strings.Join(labels, " "))
		return
	}

	fmt.Fprintf(w, "[FAIL] %s  %s\n", title, Dur(elapsed))
	width := 0
	for _, r := range results {
		width = max(width, len(r.Step.Label))
	}
	for _, r := range results {
		if r.OK {
			fmt.Fprintf(w, "  [PASS] %-*s  %s\n", width, r.Step.Label, Dur(r.Dur))
			continue
		}
		fmt.Fprintf(w, "  [FAIL] %-*s  %s  (%s)\n", width, r.Step.Label, Dur(r.Dur), exitDesc(r))
		writeExcerpt(w, r, maxLinesMulti, "    ")
	}
	if skipped > 0 {
		fmt.Fprintf(w, "  %d later step(s) not run\n", skipped)
	}
	if len(failed) > 0 {
		fmt.Fprintf(w, "%s FAIL: %s\n", title, strings.Join(failed, ", "))
	}
}

// writeExcerpt prints the salient failure lines of a result, indented, then a
// count of any elided lines and the path of the full log.
func writeExcerpt(w io.Writer, r Result, limit int, indent string) {
	lines, elided := extractFailure(r.Combined, limit)
	for _, ln := range lines {
		fmt.Fprintf(w, "%s%s\n", indent, ln)
	}
	var tail []string
	if elided > 0 {
		tail = append(tail, fmt.Sprintf("+ %d more", elided))
	}
	if r.LogPath != "" {
		tail = append(tail, "log: "+r.LogPath)
	}
	if len(tail) > 0 {
		fmt.Fprintf(w, "%s%s\n", indent, strings.Join(tail, " · "))
	}
}

// exitDesc says how a failed step ended.
func exitDesc(r Result) string {
	switch {
	case r.Reason != "":
		return r.Reason
	case r.ExitCode == 137:
		return "exit 137: killed with SIGKILL, usually because the shared qrun budget hit its memory cap (`journalctl -k | grep oom-kill` confirms). " +
			"Rerun once other runs finish, or raise the cap with `qrun config set budget.memory_max SIZE`"
	default:
		return fmt.Sprintf("exit %d", r.ExitCode)
	}
}

// Dur formats a duration compactly: "820ms" under a second, "1.2s" under a
// minute, then "3m05s".
func Dur(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		d = d.Round(time.Second)
		return fmt.Sprintf("%dm%02ds", d/time.Minute, (d%time.Minute)/time.Second)
	}
}

func ensureTrailingNL(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
