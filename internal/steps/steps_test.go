package steps

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runOne(t *testing.T, cmd string, opts Options) Result {
	t.Helper()
	t.Setenv("QRUN_NO_PROGRESS", "1")
	if opts.LogDir == "" {
		opts.LogDir = t.TempDir()
	}
	results := Run(context.Background(), []Step{{Label: "step", Cmd: cmd}}, opts)
	if len(results) != 1 {
		t.Fatalf("Run returned %d results, want 1", len(results))
	}
	return results[0]
}

func TestRunStep(t *testing.T) {
	t.Run("a passing step captures output and writes the log", func(t *testing.T) {
		r := runOne(t, "echo out; echo err >&2", Options{})
		if !r.OK || r.ExitCode != 0 {
			t.Errorf("result = %+v, want a pass", r)
		}
		if !strings.Contains(r.Combined, "out") || !strings.Contains(r.Combined, "err") {
			t.Errorf("combined = %q, want both streams", r.Combined)
		}
		logged, err := os.ReadFile(r.LogPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(logged), "out") {
			t.Errorf("log = %q", logged)
		}
	})

	t.Run("a failing step reports its exit code", func(t *testing.T) {
		r := runOne(t, "exit 3", Options{})
		if r.OK || r.ExitCode != 3 {
			t.Errorf("result = %+v, want exit 3", r)
		}
	})

	t.Run("color is switched off and stripped", func(t *testing.T) {
		r := runOne(t, `printf '\033[31m%s\033[0m\n' "$NO_COLOR"`, Options{})
		if strings.TrimSpace(r.Combined) != "1" {
			t.Errorf("combined = %q, want plain \"1\"", r.Combined)
		}
	})

	// sh often execs the last command in its own place, so a kill is seen as
	// a signal death, not as the shell's 128+n exit code. The memory-cap hint
	// keys on 137 either way.
	t.Run("a step killed by a signal reports 128 plus the signal", func(t *testing.T) {
		r := runOne(t, "kill -KILL $$", Options{})
		if r.OK || r.ExitCode != 137 {
			t.Errorf("result = %+v, want exit 137", r)
		}
	})

	t.Run("fail-nonempty fails a step that exits 0 with stdout", func(t *testing.T) {
		r := runOne(t, "echo unformatted.go", Options{FailNonempty: true})
		if r.OK || r.Reason != "produced output (expected none)" {
			t.Errorf("result = %+v", r)
		}
		r = runOne(t, "echo note >&2", Options{FailNonempty: true})
		if !r.OK {
			t.Errorf("stderr alone failed the step: %+v", r)
		}
	})

	t.Run("an unusable log dir still runs the step", func(t *testing.T) {
		r := runOne(t, "echo hi", Options{LogDir: filepath.Join(t.TempDir(), "missing")})
		if !r.OK || r.LogPath != "" {
			t.Errorf("result = %+v, want a pass with no log path", r)
		}
	})
}

func TestCancellingStopsTheStepAndItsChildren(t *testing.T) {
	t.Setenv("QRUN_NO_PROGRESS", "1")
	marker := filepath.Join(t.TempDir(), "survived")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	// The inner shell is a grandchild: it outlives a signal sent only to the
	// step's own shell.
	steps := []Step{
		{Label: "slow", Cmd: "sh -c 'sleep 5; touch " + marker + "' & wait"},
		{Label: "never", Cmd: "touch " + marker},
	}
	start := time.Now()
	results := Run(ctx, steps, Options{LogDir: t.TempDir()})

	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("Run took %s after cancel; the step was not stopped", took)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want only the interrupted step", len(results))
	}
	if r := results[0]; r.OK || r.Reason != "interrupted" || r.ExitCode != 130 {
		t.Errorf("result = %+v, want interrupted", r)
	}
	if AllOK(results, len(steps)) {
		t.Error("AllOK for a run that skipped a step")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a child of the cancelled step kept running, or a later step ran")
	}
}

func TestAStepThatLeavesAChildBehindStillReturns(t *testing.T) {
	// A daemon started by a step inherits its output pipe. Without a bound on
	// that, the run would wait for the daemon to exit.
	defer func(grace time.Duration) { stopGrace = grace }(stopGrace)
	stopGrace = 200 * time.Millisecond

	start := time.Now()
	r := runOne(t, "sleep 30 & echo started", Options{})
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("Run took %s waiting on a leftover child", took)
	}
	if !r.OK {
		t.Errorf("result = %+v, want a pass: the step itself exited 0", r)
	}
}

func TestAllOK(t *testing.T) {
	pass, fail := Result{OK: true}, Result{}
	if !AllOK([]Result{pass, pass}, 2) {
		t.Error("two passes of two is not OK")
	}
	if AllOK([]Result{pass, fail}, 2) {
		t.Error("a failure is OK")
	}
	if AllOK([]Result{pass}, 2) {
		t.Error("a skipped step is OK")
	}
}

func TestStripANSI(t *testing.T) {
	in := "\x1b[31mred\x1b[0m \x1b[1;32mbold green\x1b[0m plain"
	if got := stripANSI(in); got != "red bold green plain" {
		t.Errorf("stripANSI = %q", got)
	}
}

func TestExtractFailure(t *testing.T) {
	t.Run("tsc diagnostics", func(t *testing.T) {
		out := strings.Join([]string{
			"> @acme/web typecheck",
			"src/foo.tsx(42,5): error TS2322: Type 'string' is not assignable to 'number'.",
			"src/bar.tsx(58,9): error TS2554: Expected 2 arguments, but got 1.",
			"Found 2 errors.",
		}, "\n")
		lines, elided := extractFailure(out, maxLinesSingle)
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "TS2322") || !strings.Contains(joined, "TS2554") {
			t.Errorf("missing diagnostics:\n%s", joined)
		}
		if strings.Contains(joined, "@acme/web typecheck") {
			t.Errorf("kept noise line:\n%s", joined)
		}
		if elided != 0 {
			t.Errorf("elided = %d, want 0", elided)
		}
	})

	t.Run("go test failure with detail", func(t *testing.T) {
		out := strings.Join([]string{
			"=== RUN   TestPrice",
			"--- FAIL: TestPrice (0.00s)",
			"    price_test.go:21: expected 120, got 100",
			"FAIL",
			"FAIL\texample/pkg\t0.2s",
		}, "\n")
		lines, _ := extractFailure(out, maxLinesSingle)
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "--- FAIL: TestPrice") {
			t.Errorf("missing fail header:\n%s", joined)
		}
		if !strings.Contains(joined, "price_test.go:21") {
			t.Errorf("missing detail line:\n%s", joined)
		}
	})

	for _, tc := range []struct{ name, noise, diagnostic string }{
		{"python traceback frame", "collected 4 items", `  File "app/models.py:88", in save`},
		{"rust compiler error", "   Compiling app v0.1.0", "error[E0308]: mismatched types"},
		{"rust source location", "   Compiling app v0.1.0", "  --> src/main.rs:12:5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, _ := extractFailure(tc.noise+"\n"+tc.diagnostic+"\n", maxLinesSingle)
			if len(lines) != 1 || lines[0] != strings.TrimRight(tc.diagnostic, " ") {
				t.Errorf("lines = %q, want only the diagnostic", lines)
			}
		})
	}

	t.Run("falls back to tail when nothing matches", func(t *testing.T) {
		out := "line one\nline two\n\nline three\n"
		lines, _ := extractFailure(out, 2)
		if len(lines) != 2 || lines[len(lines)-1] != "line three" {
			t.Errorf("tail = %v", lines)
		}
	})

	t.Run("caps and reports elided count", func(t *testing.T) {
		var b strings.Builder
		for range 10 {
			b.WriteString("src/f.go:1:1: oops\n")
		}
		lines, elided := extractFailure(b.String(), 4)
		if len(lines) != 4 {
			t.Errorf("len = %d, want 4", len(lines))
		}
		if elided != 6 {
			t.Errorf("elided = %d, want 6", elided)
		}
	})
}

func render(opts Options, results []Result, want int) string {
	var b bytes.Buffer
	Render(&b, opts, results, want, 1500*time.Millisecond)
	return b.String()
}

func TestRender(t *testing.T) {
	pass := func(label string) Result {
		return Result{Step: Step{Label: label}, OK: true, Dur: 820 * time.Millisecond}
	}
	fail := func(label, output string) Result {
		return Result{Step: Step{Label: label}, ExitCode: 1, Dur: 2 * time.Second, Combined: output, LogPath: "/logs/" + label + ".log"}
	}

	t.Run("a passing single step is one line", func(t *testing.T) {
		got := render(Options{Title: "lint"}, []Result{pass("lint")}, 1)
		if got != "[PASS] lint  820ms\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("show-on-success prints the output first", func(t *testing.T) {
		r := pass("diff")
		r.Combined = "+ added line"
		got := render(Options{Title: "diff", ShowOnSuccess: true}, []Result{r}, 1)
		if got != "+ added line\n[PASS] diff  820ms\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a failing single step shows the excerpt and the log", func(t *testing.T) {
		got := render(Options{Title: "lint"}, []Result{fail("lint", "noise\nsrc/a.go:3:1: unused variable\n")}, 1)
		want := "[FAIL] lint  2.0s  (exit 1)\n  src/a.go:3:1: unused variable\n  log: /logs/lint.log\n"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("an all-passing multi-step run is one line", func(t *testing.T) {
		got := render(Options{Title: "verify"}, []Result{pass("lint"), pass("test")}, 2)
		if got != "[PASS] verify  1.5s  ·  lint test\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a failing multi-step run lists every step and names the failures", func(t *testing.T) {
		got := render(Options{Title: "verify"}, []Result{pass("lint"), fail("test", "--- FAIL: TestX (0.00s)\n")}, 2)
		for _, want := range []string{
			"[FAIL] verify  1.5s\n",
			"  [PASS] lint  820ms\n",
			"  [FAIL] test  2.0s  (exit 1)\n",
			"    --- FAIL: TestX (0.00s)\n",
			"    log: /logs/test.log\n",
			"verify FAIL: test\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
	})

	t.Run("an interrupted run says what did not run", func(t *testing.T) {
		interrupted := Result{Step: Step{Label: "lint"}, ExitCode: 130, Reason: "interrupted", Dur: time.Second}
		got := render(Options{Title: "verify"}, []Result{interrupted}, 3)
		if !strings.Contains(got, "(interrupted)") || !strings.Contains(got, "2 later step(s) not run") {
			t.Errorf("got:\n%s", got)
		}
	})

	t.Run("exit 137 names the memory cap and the command that raises it", func(t *testing.T) {
		got := exitDesc(Result{ExitCode: 137})
		for _, want := range []string{"SIGKILL", "memory cap", "qrun config set budget.memory_max"} {
			if !strings.Contains(got, want) {
				t.Errorf("exitDesc(137) = %q, want it to mention %q", got, want)
			}
		}
	})
}

func TestDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		820 * time.Millisecond:                "820ms",
		1200 * time.Millisecond:               "1.2s",
		59*time.Second + 900*time.Millisecond: "59.9s",
		3*time.Minute + 5*time.Second:         "3m05s",
		61*time.Minute + 2*time.Second:        "61m02s",
	} {
		if got := Dur(d); got != want {
			t.Errorf("Dur(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestIsTerminalOnPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminal(w) {
		t.Error("a pipe should not be reported as a terminal")
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("db:test/fancy name"); got != "db-test-fancy-name" {
		t.Errorf("sanitize = %q", got)
	}
}
