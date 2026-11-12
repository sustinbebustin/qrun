package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sustinbebustin/qrun/internal/config"
)

// settingsIn points qrun at a settings file under a fresh directory and
// returns the file's path. The file does not exist yet.
func settingsIn(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "qrun", "config.toml")
}

func writeSettings(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSettings(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustInvoke(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := invoke(args...)
	if code != exitOK {
		t.Fatalf("qrun %v: exit %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
	}
	return stdout
}

func TestConfigShow(t *testing.T) {
	t.Run("with no file, every key is at its default and the output is a valid settings file", func(t *testing.T) {
		path := settingsIn(t)
		for _, args := range [][]string{{"config"}, {"config", "show"}} {
			out := mustInvoke(t, args...)
			for _, key := range config.Keys() {
				if !strings.Contains(out, key.Name+" = ") {
					t.Errorf("qrun %v lacks %s:\n%s", args, key.Name, out)
				}
			}
			if strings.Count(out, "# default\n") != len(config.Keys()) || !strings.Contains(out, path+" (not created") {
				t.Errorf("qrun %v:\n%s", args, out)
			}
			def := config.Default(config.ThisMachine())
			if c, err := config.Parse(out, def); err != nil || c != def {
				t.Errorf("the output does not read back as the defaults: %+v, %v", c, err)
			}
		}
	})

	t.Run("names the values the file sets, with their defaults", func(t *testing.T) {
		writeSettings(t, settingsIn(t), "slots = 3\n")
		out := mustInvoke(t, "config", "show")
		if !strings.Contains(out, "slots = 3") || !strings.Contains(out, "# set in the settings file (default: 1)") {
			t.Errorf("got:\n%s", out)
		}
	})

	t.Run("--json carries the same, as text values", func(t *testing.T) {
		path := settingsIn(t)
		writeSettings(t, path, "slots = 3\n")
		var report configShowReport
		if err := json.Unmarshal([]byte(mustInvoke(t, "config", "show", "--json")), &report); err != nil {
			t.Fatal(err)
		}
		if report.Path != path || !report.Exists || report.Error != "" || len(report.Settings) != len(config.Keys()) {
			t.Errorf("report = %+v", report)
		}
		if got, want := report.Settings[0], (settingReport{Key: "slots", Value: "3", Default: "1", Source: "file"}); got != want {
			t.Errorf("slots = %+v, want %+v", got, want)
		}
		if got := report.Settings[1]; got.Key != "hold" || got.Value != "5m" || got.Source != "default" {
			t.Errorf("hold = %+v", got)
		}
	})

	t.Run("an invalid file shows the defaults in force and says why", func(t *testing.T) {
		writeSettings(t, settingsIn(t), "slots = -1\n")
		code, stdout, stderr := invoke("config", "show")
		if code != exitOK || !strings.Contains(stdout, "slots = 1 ") || !strings.Contains(stdout, "invalid, so the defaults are in force") {
			t.Errorf("exit %d, stdout:\n%s", code, stdout)
		}
		if !strings.Contains(stderr, "slots = -1: must be 0") {
			t.Errorf("stderr = %q", stderr)
		}
	})
}

func TestConfigGet(t *testing.T) {
	writeSettings(t, settingsIn(t), "[budget]\nmemory_max = \"none\"\n")
	for key, want := range map[string]string{"slots": "1\n", "hold": "5m\n", "budget.memory_max": "none\n", "budget.enabled": "true\n"} {
		if got := mustInvoke(t, "config", "get", key); got != want {
			t.Errorf("get %s = %q, want %q", key, got, want)
		}
	}
}

func TestConfigSet(t *testing.T) {
	t.Run("creates a commented file and reports the change", func(t *testing.T) {
		path := settingsIn(t)
		out := mustInvoke(t, "config", "set", "slots", "2")
		if !strings.Contains(out, "slots = 2  (was 1)\n") || !strings.Contains(out, "settings file: "+path) {
			t.Errorf("got:\n%s", out)
		}
		text := readSettings(t, path)
		if !strings.Contains(text, "Runs allowed at once, machine-wide. 0 turns the queue off.\nslots = 2\n") {
			t.Errorf("slots is not set under its explanation:\n%s", text)
		}
		if got := mustInvoke(t, "config", "get", "slots"); got != "2\n" {
			t.Errorf("get slots = %q after set", got)
		}
	})

	t.Run("takes several KEY=VALUE at once", func(t *testing.T) {
		settingsIn(t)
		mustInvoke(t, "config", "set", "budget.cpus=6", "budget.memory_max=8G", "hold=0")
		for key, want := range map[string]string{"budget.cpus": "6\n", "budget.memory_max": "8G\n", "hold": "0\n"} {
			if got := mustInvoke(t, "config", "get", key); got != want {
				t.Errorf("get %s = %q, want %q", key, got, want)
			}
		}
	})

	t.Run("run twice, the second changes nothing", func(t *testing.T) {
		path := settingsIn(t)
		mustInvoke(t, "config", "set", "slots", "2")
		first := readSettings(t, path)
		out := mustInvoke(t, "config", "set", "slots", "2")
		if !strings.Contains(out, "slots = 2  (unchanged)\n") || !strings.Contains(out, "(not changed)") {
			t.Errorf("got:\n%s", out)
		}
		if readSettings(t, path) != first {
			t.Error("the file was rewritten")
		}
	})

	t.Run("keeps a hand-written file's comments", func(t *testing.T) {
		path := settingsIn(t)
		writeSettings(t, path, "# tuned for the build box\nslots = 2 # two at once\n")
		mustInvoke(t, "config", "set", "slots", "4")
		if got, want := readSettings(t, path), "# tuned for the build box\nslots = 4 # two at once\n"; got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("--dry-run reports the change and writes nothing", func(t *testing.T) {
		path := settingsIn(t)
		out := mustInvoke(t, "config", "set", "--dry-run", "slots", "2")
		if !strings.Contains(out, "slots = 2  (was 1)\n") || !strings.Contains(out, "dry run: "+path+" not written") {
			t.Errorf("got:\n%s", out)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the file was created (stat error: %v)", err)
		}
	})

	t.Run("a bad value or key is a usage error that changes nothing", func(t *testing.T) {
		path := settingsIn(t)
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"config", "set", "slots", "-1"}, "example: qrun config set slots 2"},
			{[]string{"config", "set", "budget.memory_max", "1.5G"}, "example: qrun config set budget.memory_max 16G"},
			{[]string{"config", "set", "slot", "2"}, "known: slots, hold, time_limit, budget.enabled"},
			{[]string{"config", "set", "slots=2", "budget.cpus"}, "KEY=VALUE"},
			{[]string{"config", "set"}, "nothing to set"},
			{[]string{"config", "set", "--force", "slots", "2"}, "unknown flag"},
		} {
			code, stdout, stderr := invoke(tc.args...)
			if code != exitUsage || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Errorf("qrun %v: exit %d, stdout %q, stderr:\n%s", tc.args, code, stdout, stderr)
			}
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("a rejected set created the file (stat error: %v)", err)
		}
	})

	t.Run("fixes the key it sets and fails while the file has other problems", func(t *testing.T) {
		path := settingsIn(t)
		writeSettings(t, path, "slots = -1\nbogus = 1\n")
		code, stdout, stderr := invoke("config", "set", "slots", "2")
		if code != exitFailed || !strings.Contains(stdout, "slots = 2") || !strings.Contains(stderr, "bogus: unknown key") {
			t.Errorf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if got, want := readSettings(t, path), "slots = 2\nbogus = 1\n"; got != want {
			t.Errorf("file = %q, want %q", got, want)
		}
	})

	t.Run("refuses a file it cannot edit safely, and leaves it alone", func(t *testing.T) {
		path := settingsIn(t)
		const text = "budget = { cpus = 4 }\n"
		writeSettings(t, path, text)
		code, _, stderr := invoke("config", "set", "budget.cpus", "6")
		if code != exitFailed || !strings.Contains(stderr, "Nothing was changed") || !strings.Contains(stderr, path) {
			t.Errorf("exit %d, stderr:\n%s", code, stderr)
		}
		if readSettings(t, path) != text {
			t.Error("the file was changed")
		}
	})
}

func TestConfigUnset(t *testing.T) {
	t.Run("puts a key back to its default", func(t *testing.T) {
		path := settingsIn(t)
		writeSettings(t, path, "slots = 3\n[budget]\ncpus = 4\n")
		out := mustInvoke(t, "config", "unset", "slots")
		if !strings.Contains(out, "slots = 1  (was 3)\n") {
			t.Errorf("got:\n%s", out)
		}
		if got, want := readSettings(t, path), "# slots = 1\n[budget]\ncpus = 4\n"; got != want {
			t.Errorf("file = %q, want %q", got, want)
		}
	})

	t.Run("with no file there is nothing to do", func(t *testing.T) {
		path := settingsIn(t)
		out := mustInvoke(t, "config", "unset", "slots")
		if !strings.Contains(out, "slots = 1  (unchanged)\n") {
			t.Errorf("got:\n%s", out)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("unset created the file (stat error: %v)", err)
		}
	})
}

func TestConfigInit(t *testing.T) {
	path := settingsIn(t)

	out := mustInvoke(t, "config", "init", "--dry-run")
	if _, err := os.Stat(path); !os.IsNotExist(err) || !strings.Contains(out, "# slots = 1\n") {
		t.Errorf("a dry run should print the file and not write it (stat error: %v):\n%s", err, out)
	}

	if out := mustInvoke(t, "config", "init"); !strings.Contains(out, "created: "+path) {
		t.Errorf("got:\n%s", out)
	}
	created := readSettings(t, path)
	if set := config.InFile(created); len(set) != 0 {
		t.Errorf("the new file sets %v, want nothing", set)
	}

	mustInvoke(t, "config", "set", "slots", "2")
	if out := mustInvoke(t, "config", "init"); !strings.Contains(out, "already exists: "+path) {
		t.Errorf("got:\n%s", out)
	}
	if got := mustInvoke(t, "config", "get", "slots"); got != "2\n" {
		t.Errorf("a second init reset slots to %q", got)
	}
}

func TestConfigCheck(t *testing.T) {
	path := settingsIn(t)
	if out := mustInvoke(t, "config", "check"); !strings.Contains(out, "does not exist, so the defaults are in force") {
		t.Errorf("got:\n%s", out)
	}

	writeSettings(t, path, "slots = 2\n")
	if out := mustInvoke(t, "config", "check"); out != "ok: "+path+"\n" {
		t.Errorf("got %q", out)
	}

	candidate := filepath.Join(t.TempDir(), "candidate.toml")
	writeSettings(t, candidate, "slots = -1\n[budget]\ncpus = -2\n")
	code, stdout, stderr := invoke("config", "check", candidate)
	if code != exitFailed || stdout != "" || !strings.Contains(stderr, "slots = -1") || !strings.Contains(stderr, "budget.cpus = -2") {
		t.Errorf("exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}

	if code, _, stderr := invoke("config", "check", filepath.Join(t.TempDir(), "absent.toml")); code != exitFailed || !strings.Contains(stderr, "cannot read") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestConfigPath(t *testing.T) {
	path := settingsIn(t)
	if got := mustInvoke(t, "config", "path"); got != path+"\n" {
		t.Errorf("path = %q, want %q", got, path)
	}
}

func TestConfigHelpListsEveryKey(t *testing.T) {
	out := mustInvoke(t, "config", "--help")
	for _, key := range config.Keys() {
		if !strings.Contains(out, "\n  "+key.Name+"\n") {
			t.Errorf("help lacks the key %s", key.Name)
		}
	}
}
