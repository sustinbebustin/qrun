package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	t.Run("one slot, half the cpus, half of RAM with no swap", func(t *testing.T) {
		c := Default(Machine{CPUs: 20, MemoryKiB: 24_595_128})
		want := Config{Slots: 1, Budget: Budget{Enabled: true, CPUs: 10, MemoryMax: "12297564K"}}
		if c != want {
			t.Errorf("Default = %+v, want %+v", c, want)
		}
	})

	t.Run("rounds odd cpu counts up and never below one", func(t *testing.T) {
		if c := Default(Machine{CPUs: 3}); c.Budget.CPUs != 2 {
			t.Errorf("cpus = %d, want 2", c.Budget.CPUs)
		}
		if c := Default(Machine{CPUs: 1}); c.Budget.CPUs != 1 {
			t.Errorf("cpus = %d, want 1", c.Budget.CPUs)
		}
	})

	t.Run("unknown memory leaves the memory cap off", func(t *testing.T) {
		if c := Default(Machine{CPUs: 8}); c.Budget.MemoryMax != "" {
			t.Errorf("MemoryMax = %q, want none", c.Budget.MemoryMax)
		}
	})
}

func TestParse(t *testing.T) {
	def := Default(Machine{CPUs: 20, MemoryKiB: 24_595_128})

	t.Run("an empty file keeps every default", func(t *testing.T) {
		c, err := Parse("", def)
		if err != nil {
			t.Fatal(err)
		}
		if c != def {
			t.Errorf("Parse(\"\") = %+v, want %+v", c, def)
		}
	})

	t.Run("each key overrides only itself", func(t *testing.T) {
		c, err := Parse("slots = 2\n[budget]\ncpus = 6\n", def)
		if err != nil {
			t.Fatal(err)
		}
		want := def
		want.Slots = 2
		want.Budget.CPUs = 6
		if c != want {
			t.Errorf("got %+v, want %+v", c, want)
		}
	})

	t.Run("sizes take systemd suffixes and percentages", func(t *testing.T) {
		c, err := Parse("[budget]\nmemory_max = \"10G\"\nmemory_high = \"40%\"\n", def)
		if err != nil {
			t.Fatal(err)
		}
		if c.Budget.MemoryMax != "10G" || c.Budget.MemoryHigh != "40%" {
			t.Errorf("budget = %+v", c.Budget)
		}
	})

	t.Run("none removes a memory limit", func(t *testing.T) {
		c, err := Parse("[budget]\nmemory_max = \"none\"\n", def)
		if err != nil {
			t.Fatal(err)
		}
		if c.Budget.MemoryMax != "" {
			t.Errorf("MemoryMax = %q, want none", c.Budget.MemoryMax)
		}
	})

	t.Run("zero slots and zero cpus are valid ways to switch a limit off", func(t *testing.T) {
		c, err := Parse("slots = 0\n[budget]\ncpus = 0\nenabled = false\n", def)
		if err != nil {
			t.Fatal(err)
		}
		if c.Slots != 0 || c.Budget.CPUs != 0 || c.Budget.Enabled {
			t.Errorf("got %+v", c)
		}
	})

	for _, tc := range []struct {
		name, toml, wantInErr string
	}{
		{"negative slots", "slots = -1", "slots"},
		{"negative cpus", "[budget]\ncpus = -2", "budget.cpus"},
		{"fractional size", "[budget]\nmemory_max = \"1.5G\"", "budget.memory_max"},
		{"size with unknown suffix", "[budget]\nmemory_high = \"12GB\"", "budget.memory_high"},
		{"percentage over 100", "[budget]\nmemory_max = \"150%\"", "budget.memory_max"},
		{"unknown key", "slot = 2", "slot"},
		{"unknown budget key", "[budget]\nmem_max = \"1G\"", "budget.mem_max"},
		{"wrong type", "slots = \"two\"", "slots"},
		{"not toml", "slots = = 2", "line 1"},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			c, err := Parse(tc.toml, def)
			if err == nil {
				t.Fatalf("Parse(%q) = %+v, want an error", tc.toml, c)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error %q does not name %q", err, tc.wantInErr)
			}
		})
	}

	t.Run("reports every problem at once", func(t *testing.T) {
		_, err := Parse("slots = -1\n[budget]\ncpus = -2\n", def)
		if err == nil || !strings.Contains(err.Error(), "slots") || !strings.Contains(err.Error(), "budget.cpus") {
			t.Errorf("error = %v, want both slots and budget.cpus named", err)
		}
	})
}

func TestLoad(t *testing.T) {
	def := Default(Machine{CPUs: 4})

	t.Run("a missing file is the defaults, not an error", func(t *testing.T) {
		c, err := Load(filepath.Join(t.TempDir(), "absent.toml"), def)
		if err != nil {
			t.Fatal(err)
		}
		if c != def {
			t.Errorf("got %+v, want defaults", c)
		}
	})

	t.Run("reads the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("slots = 3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path, def)
		if err != nil {
			t.Fatal(err)
		}
		if c.Slots != 3 {
			t.Errorf("slots = %d, want 3", c.Slots)
		}
	})

	t.Run("an invalid file names its path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("slots = -1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path, def); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("error = %v, want it to name %s", err, path)
		}
	})
}

func TestPath(t *testing.T) {
	t.Run("honours XDG_CONFIG_HOME", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/xdg")
		if got := Path(); got != "/xdg/qrun/config.toml" {
			t.Errorf("Path = %q", got)
		}
	})

	t.Run("falls back to ~/.config", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "/home/someone")
		if got := Path(); got != "/home/someone/.config/qrun/config.toml" {
			t.Errorf("Path = %q", got)
		}
	})
}

func TestReadMemTotal(t *testing.T) {
	meminfo := "MemFree:  100 kB\nMemTotal:       24595128 kB\nSwapTotal: 8 kB\n"
	if got := parseMemTotalKiB(meminfo); got != 24_595_128 {
		t.Errorf("parseMemTotalKiB = %d", got)
	}
	if got := parseMemTotalKiB("nothing useful"); got != 0 {
		t.Errorf("parseMemTotalKiB of junk = %d, want 0", got)
	}
}
