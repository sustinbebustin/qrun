// Package config holds the machine-wide qrun settings.
//
// Every qrun process on a machine shares one queue and one resource budget,
// so the settings that shape them live in one file rather than in each
// caller's environment: two callers that disagreed about the slot count or
// the memory cap would each be enforcing a different limit on the same
// machine.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the resolved settings: the file's values over the defaults.
type Config struct {
	// Slots is how many queued runs may execute at once, machine-wide.
	// 0 switches the queue off.
	Slots  int
	Budget Budget
}

// Budget is the resource ceiling every run shares.
type Budget struct {
	// Enabled is false when runs should not be placed under any limit.
	Enabled bool
	// CPUs is the CPU quota in whole cores. 0 means no quota.
	CPUs int
	// MemoryMax is the hard memory cap in systemd size syntax, enforced with
	// swap off so exceeding it kills one step rather than thrashing the
	// machine. Empty means no cap.
	MemoryMax string
	// MemoryHigh is the soft memory limit in systemd size syntax. Empty means
	// none, which is the default: a soft limit below MemoryMax throttles a
	// run into continuous reclaim instead of letting it finish or fail.
	MemoryHigh string
}

// Machine is the hardware the defaults are derived from.
type Machine struct {
	CPUs      int
	MemoryKiB int64 // 0 when unknown
}

// Default returns the settings used when the file sets nothing: one run at a
// time, on half the CPUs and half the memory, leaving the other half for
// interactive work.
func Default(m Machine) Config {
	c := Config{Slots: 1, Budget: Budget{Enabled: true, CPUs: max((m.CPUs+1)/2, 1)}}
	if m.MemoryKiB > 0 {
		c.Budget.MemoryMax = strconv.FormatInt(m.MemoryKiB/2, 10) + "K"
	}
	return c
}

// ThisMachine reads the CPU count and total memory of the host.
func ThisMachine() Machine {
	m := Machine{CPUs: runtime.NumCPU()}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		m.MemoryKiB = parseMemTotalKiB(string(data))
	}
	return m
}

// parseMemTotalKiB extracts MemTotal from /proc/meminfo text; 0 when absent.
func parseMemTotalKiB(meminfo string) int64 {
	for line := range strings.Lines(meminfo) {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}

// Path is where the settings file lives: $XDG_CONFIG_HOME/qrun/config.toml,
// or ~/.config/qrun/config.toml.
func Path() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(base, "qrun", "config.toml")
}

// Load reads the settings file at path over def. A missing file is not an
// error: it means the defaults.
func Load(path string, def Config) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return def, nil
	}
	if err != nil {
		return def, fmt.Errorf("read %s: %w", path, err)
	}
	c, err := Parse(string(data), def)
	if err != nil {
		// One problem per line under the path, since Parse reports them all.
		return def, fmt.Errorf("%s:\n  %s", path, strings.ReplaceAll(err.Error(), "\n", "\n  "))
	}
	return c, nil
}

// file is the on-disk shape. Pointers distinguish "not set" from a zero value,
// since 0 slots and 0 cpus both mean something.
type file struct {
	Slots  *int `toml:"slots"`
	Budget struct {
		Enabled    *bool   `toml:"enabled"`
		CPUs       *int    `toml:"cpus"`
		MemoryMax  *string `toml:"memory_max"`
		MemoryHigh *string `toml:"memory_high"`
	} `toml:"budget"`
}

// Parse applies TOML text over def. It reports every invalid or unknown key
// together, so a file is fixed in one pass.
func Parse(text string, def Config) (Config, error) {
	var f file
	md, err := toml.Decode(text, &f)
	if err != nil {
		return def, parseError(err)
	}

	c := def
	var problems []error
	for _, key := range md.Undecoded() {
		problems = append(problems, fmt.Errorf("%s: unknown key (known: slots, budget.enabled, budget.cpus, budget.memory_max, budget.memory_high)", key))
	}
	if f.Slots != nil {
		if *f.Slots < 0 {
			problems = append(problems, fmt.Errorf("slots = %d: must be 0 (no queue) or more", *f.Slots))
		}
		c.Slots = *f.Slots
	}
	if f.Budget.Enabled != nil {
		c.Budget.Enabled = *f.Budget.Enabled
	}
	if f.Budget.CPUs != nil {
		if *f.Budget.CPUs < 0 {
			problems = append(problems, fmt.Errorf("budget.cpus = %d: must be 0 (no CPU limit) or more", *f.Budget.CPUs))
		}
		c.Budget.CPUs = *f.Budget.CPUs
	}
	if f.Budget.MemoryMax != nil {
		size, err := parseSize(*f.Budget.MemoryMax)
		if err != nil {
			problems = append(problems, fmt.Errorf("budget.memory_max = %q: %w", *f.Budget.MemoryMax, err))
		}
		c.Budget.MemoryMax = size
	}
	if f.Budget.MemoryHigh != nil {
		size, err := parseSize(*f.Budget.MemoryHigh)
		if err != nil {
			problems = append(problems, fmt.Errorf("budget.memory_high = %q: %w", *f.Budget.MemoryHigh, err))
		}
		c.Budget.MemoryHigh = size
	}
	if len(problems) > 0 {
		return def, errors.Join(problems...)
	}
	return c, nil
}

// parseError keeps the TOML decoder's line number and message without its
// multi-line source excerpt.
func parseError(err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		return fmt.Errorf("line %d: %s", pe.Position.Line, pe.Message)
	}
	return err
}

var (
	bytesRE   = regexp.MustCompile(`^\d+[KMGT]?$`)
	percentRE = regexp.MustCompile(`^(\d+)%$`)
)

// parseSize validates a memory size and returns it in the form systemd takes.
// "none" (or an empty string) means no limit and returns "".
func parseSize(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "" || s == "none":
		return "", nil
	case bytesRE.MatchString(s):
		return s, nil
	}
	if m := percentRE.FindStringSubmatch(s); m != nil {
		if pct, err := strconv.Atoi(m[1]); err == nil && pct >= 1 && pct <= 100 {
			return s, nil
		}
	}
	return "", errors.New(`want a whole number with an optional K, M, G or T suffix (such as "12G"), a percentage of RAM from "1%" to "100%", or "none"`)
}
