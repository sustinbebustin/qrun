package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var editDef = Default(Machine{CPUs: 20, MemoryKiB: 24_595_128})

func mustLookup(t *testing.T, name string) Key {
	t.Helper()
	k, ok := Lookup(name)
	if !ok {
		t.Fatalf("no setting named %q", name)
	}
	return k
}

func mustSet(t *testing.T, text, name, value string) string {
	t.Helper()
	out, err := Set(text, mustLookup(t, name), value)
	if err != nil {
		t.Fatalf("Set(%s = %s): %v\nin:\n%s", name, value, err, text)
	}
	return out
}

func TestTemplate(t *testing.T) {
	text := Template(editDef)

	t.Run("sets nothing, so it reads as the defaults", func(t *testing.T) {
		c, err := Parse(text, editDef)
		if err != nil {
			t.Fatal(err)
		}
		if c != editDef {
			t.Errorf("Parse(Template) = %+v, want the defaults", c)
		}
		if set := InFile(text); len(set) != 0 {
			t.Errorf("template sets %v, want nothing", set)
		}
	})

	t.Run("shows every key with its default", func(t *testing.T) {
		for _, want := range []string{"# slots = 1\n", "# hold = \"5m\"\n", "[budget]\n", "# enabled = true\n", "# cpus = 10\n", "# memory_max = \"12297564K\"\n", "# memory_high = \"none\"\n"} {
			if !strings.Contains(text, want) {
				t.Errorf("template lacks %q:\n%s", want, text)
			}
		}
	})
}

// sampleValues is a valid, non-default value for every key, in its plain form.
var sampleValues = map[string]string{
	"slots":              "3",
	"hold":               "1m30s",
	"budget.enabled":     "false",
	"budget.cpus":        "6",
	"budget.memory_max":  "8G",
	"budget.memory_high": "40%",
}

func TestSetReadsBackInEveryLayout(t *testing.T) {
	layouts := map[string]string{
		"an empty file":           "",
		"the template":            Template(editDef),
		"only a comment":          "# my settings\n",
		"a top-level key":         "slots = 2\n",
		"a budget table":          "[budget]\ncpus = 4 # for the big build\n",
		"both, with comments":     "# mine\nslots = 2 # two\n\n# limits\n[budget]\n  cpus = 4\n  memory_max = \"10G\"\n",
		"a table with no keys":    "[budget]\n",
		"a comment on the table":  "# hello\n\n# the limits\n[budget]\nenabled = true\n",
		"no trailing newline":     "slots = 2",
		"single-quoted strings":   "hold = '1m'\n[budget]\nmemory_max = '10G'\n",
		"an invalid value to fix": "slots = -1\n[budget]\ncpus = -3\nmemory_max = \"lots\"\n",
	}
	for name, layout := range layouts {
		for _, k := range Keys() {
			t.Run(name+"/"+k.Name, func(t *testing.T) {
				before := InFile(layout)
				after := InFile(mustSet(t, layout, k.Name, sampleValues[k.Name]))
				if after[k.Name] != sampleValues[k.Name] {
					t.Errorf("%s reads back as %q, want %q", k.Name, after[k.Name], sampleValues[k.Name])
				}
				for other, was := range before {
					if other != k.Name && after[other] != was {
						t.Errorf("%s changed from %q to %q", other, was, after[other])
					}
				}
				if len(after) != len(before) && len(after) != len(before)+1 {
					t.Errorf("file sets %v, was %v", after, before)
				}
			})
		}
	}
}

func TestSet(t *testing.T) {
	t.Run("fills in the template under the key's explanation", func(t *testing.T) {
		got := mustSet(t, Template(editDef), "budget.cpus", "6")
		if !strings.Contains(got, "Default: half the cores.\ncpus = 6\n") {
			t.Errorf("cpus was not set in place:\n%s", got)
		}
		if strings.Contains(got, "# cpus =") {
			t.Errorf("the commented default is still there:\n%s", got)
		}
	})

	t.Run("keeps the line's indentation and comment, and every other line", func(t *testing.T) {
		got := mustSet(t, "# mine\nslots = 2 # two\n\n[budget]\n  cpus = 4  # big build\n", "budget.cpus", "6")
		want := "# mine\nslots = 2 # two\n\n[budget]\n  cpus = 6  # big build\n"
		if got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("leaves a person's note alone, even one that looks like a setting", func(t *testing.T) {
		got := mustSet(t, "# slots = 4 ran out of memory in March; keep it low\n\n[budget]\ncpus = 4\n", "slots", "2")
		if want := "# slots = 4 ran out of memory in March; keep it low\n\nslots = 2\n\n[budget]\ncpus = 4\n"; got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("adds a key after the table's last setting", func(t *testing.T) {
		got := mustSet(t, "[budget]\ncpus = 4\n\n[other]\n", "budget.memory_max", "8G")
		if !strings.HasPrefix(got, "[budget]\ncpus = 4\nmemory_max = \"8G\"\n") {
			t.Errorf("got:\n%s", got)
		}
	})

	t.Run("adds the table when the file has none", func(t *testing.T) {
		if got, want := mustSet(t, "slots = 2\n", "budget.cpus", "6"), "slots = 2\n\n[budget]\ncpus = 6\n"; got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("puts a top-level key above the first table and its comment", func(t *testing.T) {
		got := mustSet(t, "# mine\n\n# the limits\n[budget]\ncpus = 4\n", "slots", "2")
		if want := "# mine\n\nslots = 2\n\n# the limits\n[budget]\ncpus = 4\n"; got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("writes the value in its plain form", func(t *testing.T) {
		for _, tc := range []struct{ name, value, want string }{
			{"hold", "300s", "hold = \"5m\"\n"},
			{"hold", "0s", "hold = \"0\"\n"},
			{"slots", "02", "slots = 2\n"},
			{"budget.memory_max", " none ", "[budget]\nmemory_max = \"none\"\n"},
		} {
			if got := mustSet(t, "", tc.name, tc.value); got != tc.want {
				t.Errorf("Set(%s = %q) = %q, want %q", tc.name, tc.value, got, tc.want)
			}
		}
	})

	t.Run("leaves a file that already has the value untouched", func(t *testing.T) {
		text := "slots   =   2   # as I like it"
		if got := mustSet(t, text, "slots", "2"); got != text {
			t.Errorf("got %q, want the text back unchanged", got)
		}
	})

	t.Run("rejects a value the key does not take", func(t *testing.T) {
		for name, value := range map[string]string{
			"slots":              "-1",
			"hold":               "soon",
			"budget.enabled":     "yes",
			"budget.cpus":        "half",
			"budget.memory_max":  "1.5G",
			"budget.memory_high": "150%",
		} {
			_, err := Set("", mustLookup(t, name), value)
			if _, ok := errors.AsType[*ValueError](err); !ok {
				t.Errorf("Set(%s = %s) error = %v, want a *ValueError", name, value, err)
			} else if !strings.Contains(err.Error(), name+" = ") {
				t.Errorf("error %q does not name the assignment", err)
			}
		}
	})

	t.Run("refuses a file it cannot read back", func(t *testing.T) {
		for name, text := range map[string]string{
			"not TOML":        "slots = = 2\n",
			"an inline table": "budget = { cpus = 4 }\n",
			"a dotted key":    "budget.cpus = 4\n",
		} {
			if got, err := Set(text, mustLookup(t, "budget.cpus"), "6"); err == nil {
				t.Errorf("%s: Set = %q, want an error", name, got)
			}
		}
	})
}

func TestUnset(t *testing.T) {
	t.Run("comments the line out, shows the default and keeps the line's comment", func(t *testing.T) {
		got, err := Unset("slots = 3 # the build box takes three\n[budget]\ncpus = 4\n", mustLookup(t, "slots"), editDef)
		if err != nil {
			t.Fatal(err)
		}
		if want := "# slots = 1 # the build box takes three\n[budget]\ncpus = 4\n"; got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("undoes a Set on the template", func(t *testing.T) {
		template := Template(editDef)
		for _, k := range Keys() {
			got, err := Unset(mustSet(t, template, k.Name, sampleValues[k.Name]), k, editDef)
			if err != nil {
				t.Fatal(err)
			}
			if got != template {
				t.Errorf("%s: unset after set differs from the template:\n%s", k.Name, got)
			}
		}
	})

	t.Run("leaves a file that does not set the key untouched", func(t *testing.T) {
		text := "# slots = 4\n"
		got, err := Unset(text, mustLookup(t, "slots"), editDef)
		if err != nil {
			t.Fatal(err)
		}
		if got != text {
			t.Errorf("got %q, want the text back unchanged", got)
		}
	})

	t.Run("refuses a layout it cannot read back", func(t *testing.T) {
		if got, err := Unset("budget.cpus = 4\n", mustLookup(t, "budget.cpus"), editDef); err == nil {
			t.Errorf("Unset = %q, want an error", got)
		}
	})
}

func TestWrite(t *testing.T) {
	t.Run("creates the file and its directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "qrun", "config.toml")
		if err := Write(path, "slots = 2\n"); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "slots = 2\n" {
			t.Errorf("file = %q, %v", got, err)
		}
	})

	t.Run("replaces the file, keeps its mode and leaves nothing else behind", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte("slots = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Write(path, "slots = 2\n"); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(path); string(got) != "slots = 2\n" {
			t.Errorf("file = %q", got)
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %04o, want the file's own 0600", info.Mode().Perm())
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 {
			t.Errorf("directory holds %d entries, want only the settings file", len(entries))
		}
	})

	// A settings file kept in a dotfiles repository.
	t.Run("writes through a symlink instead of replacing it", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "dotfiles", "config.toml")
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("slots = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "config.toml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := Write(link, "slots = 2\n"); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the link was replaced by a file (mode %v, err %v)", info.Mode(), err)
		}
		if got, _ := os.ReadFile(target); string(got) != "slots = 2\n" {
			t.Errorf("target = %q, want the new settings", got)
		}
	})
}
