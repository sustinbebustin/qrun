// Package config holds the machine-wide qrun settings.
//
// Every qrun process on a machine shares one queue and one resource budget,
// so the settings that shape them live in one file rather than in each
// caller's environment: two callers that disagreed about the slot count or
// the memory cap would each be enforcing a different limit on the same
// machine. `qrun config` changes a setting by editing that file, never by
// layering a value over it.
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
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the resolved settings: the file's values over the defaults.
type Config struct {
	// Slots is how many queued runs may execute at once, machine-wide.
	// 0 switches the queue off.
	Slots int
	// Hold is how long the place of a run that left the line before it was
	// admitted is kept for a rerun of the same command. 0 gives the place up
	// at once.
	Hold time.Duration
	// TimeLimit is the longest a run may hold a slot before it is stopped,
	// so that a hung command cannot block the line for good. 0 sets no limit.
	TimeLimit time.Duration
	Budget    Budget
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

// defaultHold outlasts the pause between an agent's command timing out and
// the agent running the command again.
const defaultHold = 5 * time.Minute

// Default returns the settings used when the file sets nothing: one run at a
// time, on half the CPUs and half the memory, leaving the other half for
// interactive work.
func Default(m Machine) Config {
	c := Config{Slots: 1, Hold: defaultHold, Budget: Budget{Enabled: true, CPUs: max((m.CPUs+1)/2, 1)}}
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
	Slots     *int    `toml:"slots"`
	Hold      *string `toml:"hold"`
	TimeLimit *string `toml:"time_limit"`
	Budget    struct {
		Enabled    *bool   `toml:"enabled"`
		CPUs       *int    `toml:"cpus"`
		MemoryMax  *string `toml:"memory_max"`
		MemoryHigh *string `toml:"memory_high"`
	} `toml:"budget"`
}

// Key is one setting: its name in the settings file, and its value as text.
// Text is the form `qrun config` takes and prints: a whole number, true or
// false, a size or a duration, with "none" for no memory limit.
type Key struct {
	// Name is the key as `qrun config` and error messages spell it, with its
	// table: "slots", "budget.cpus".
	Name string
	// Doc explains the setting, one line of the generated file's comment per
	// element.
	Doc []string
	// Example is a valid value, for a message that shows a correct command.
	Example string

	// quoted is true when the file holds the value as a TOML string.
	quoted bool
	get    func(Config) string
	set    func(*Config, string) error
	// raw is the value a decoded file sets, as text; false when it sets none.
	raw func(*file) (string, bool)
}

// Value is the setting's value in c, as text.
func (k Key) Value(c Config) string { return k.get(c) }

// literal is text as the TOML value the file holds.
func (k Key) literal(text string) string {
	if k.quoted {
		return strconv.Quote(text)
	}
	return text
}

// Assignment is the line that sets the key to text, as `qrun config show`
// prints it and the file holds it under the key's table.
func (k Key) Assignment(text string) string {
	return k.Name + " = " + k.literal(text)
}

// keys is every setting, in the order the file and `qrun config show` list
// them. A table's keys are adjacent.
var keys = []Key{
	countKey("slots", "2", "no queue", []string{
		"Runs allowed at once, machine-wide. 0 turns the queue off.",
	}, func(c *Config) *int { return &c.Slots }, func(f *file) *int { return f.Slots }),
	durationKey("hold", "10m", "give a place up at once", []string{
		"How long a cancelled run's place in line is kept for a rerun of the same",
		`command from the same directory. A duration such as "90s" or "5m"; "0" gives`,
		"the place up at once.",
	}, func(c *Config) *time.Duration { return &c.Hold }, func(f *file) *string { return f.Hold }),
	durationKey("time_limit", "30m", "no limit", []string{
		"The longest a run may hold a slot before qrun stops it, so that a hung command",
		`cannot block the line for good. A duration such as "30m" or "2h"; "0" sets no`,
		"limit. Time spent waiting in line does not count.",
	}, func(c *Config) *time.Duration { return &c.TimeLimit }, func(f *file) *string { return f.TimeLimit }),
	{
		Name:    "budget.enabled",
		Doc:     []string{"false runs everything with no CPU or memory limit."},
		Example: "false",
		get:     func(c Config) string { return strconv.FormatBool(c.Budget.Enabled) },
		set: func(c *Config, text string) error {
			switch text {
			case "true":
				c.Budget.Enabled = true
			case "false":
				c.Budget.Enabled = false
			default:
				return errors.New("want true or false")
			}
			return nil
		},
		raw: func(f *file) (string, bool) {
			if f.Budget.Enabled == nil {
				return "", false
			}
			return strconv.FormatBool(*f.Budget.Enabled), true
		},
	},
	countKey("budget.cpus", "8", "no CPU limit", []string{
		"CPU quota in whole cores. 0 means no CPU limit. Default: half the cores.",
	}, func(c *Config) *int { return &c.Budget.CPUs }, func(f *file) *int { return f.Budget.CPUs }),
	sizeKey("budget.memory_max", "16G", []string{
		`Hard memory cap, enforced with swap off. "none" removes it. Default: half of RAM.`,
		`A whole number with K, M, G or T, or a percentage of RAM such as "50%".`,
	}, func(c *Config) *string { return &c.Budget.MemoryMax }, func(f *file) *string { return f.Budget.MemoryMax }),
	sizeKey("budget.memory_high", "12G", []string{
		`Soft limit that throttles before the cap, in the same form. Default: "none":`,
		"below the cap it slows a run into continuous reclaim instead of letting it",
		"finish or fail.",
	}, func(c *Config) *string { return &c.Budget.MemoryHigh }, func(f *file) *string { return f.Budget.MemoryHigh }),
}

// countKey is a setting that is a whole number, where 0 switches a limit off;
// zero names what 0 means.
func countKey(name, example, zero string, doc []string, field func(*Config) *int, raw func(*file) *int) Key {
	return Key{
		Name:    name,
		Doc:     doc,
		Example: example,
		get:     func(c Config) string { return strconv.Itoa(*field(&c)) },
		set: func(c *Config, text string) error {
			n, err := strconv.Atoi(text)
			if err != nil {
				return fmt.Errorf("want a whole number, 0 (%s) or more", zero)
			}
			if n < 0 {
				return fmt.Errorf("must be 0 (%s) or more", zero)
			}
			*field(c) = n
			return nil
		},
		raw: func(f *file) (string, bool) {
			p := raw(f)
			if p == nil {
				return "", false
			}
			return strconv.Itoa(*p), true
		},
	}
}

// durationKey is a setting that is a length of time, where "0" switches it
// off; zero names what "0" does.
func durationKey(name, example, zero string, doc []string, field func(*Config) *time.Duration, raw func(*file) *string) Key {
	return Key{
		Name:    name,
		Doc:     doc,
		Example: example,
		quoted:  true,
		get:     func(c Config) string { return FormatDuration(*field(&c)) },
		set: func(c *Config, text string) error {
			d, err := time.ParseDuration(text)
			if err != nil || d < 0 {
				return fmt.Errorf(`want a duration such as "90s", "5m" or "1h", or "0" to %s`, zero)
			}
			*field(c) = d
			return nil
		},
		raw: func(f *file) (string, bool) { return deref(raw(f)) },
	}
}

// sizeKey is a setting that is a memory size, where "none" means no limit.
func sizeKey(name, example string, doc []string, field func(*Config) *string, raw func(*file) *string) Key {
	return Key{
		Name:    name,
		Doc:     doc,
		Example: example,
		quoted:  true,
		get: func(c Config) string {
			if size := *field(&c); size != "" {
				return size
			}
			return "none"
		},
		set: func(c *Config, text string) error {
			size, err := parseSize(text)
			if err != nil {
				return err
			}
			*field(c) = size
			return nil
		},
		raw: func(f *file) (string, bool) { return deref(raw(f)) },
	}
}

func deref(s *string) (string, bool) {
	if s == nil {
		return "", false
	}
	return *s, true
}

// Keys returns every setting, in the order the file lists them.
func Keys() []Key {
	return keys
}

// Lookup finds a setting by name.
func Lookup(name string) (Key, bool) {
	for _, k := range keys {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

// Names lists every setting's name, for messages that say what is known.
func Names() string {
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.Name
	}
	return strings.Join(names, ", ")
}

// decode reads TOML text into the values it sets, by key name and as text,
// and the keys in it that are not settings. It fails only when the text is
// not TOML or a value has the wrong type; the values are not validated.
func decode(text string) (values map[string]string, unknown []string, err error) {
	var f file
	md, err := toml.Decode(text, &f)
	if err != nil {
		return nil, nil, parseError(err)
	}
	values = make(map[string]string)
	for _, k := range keys {
		if text, ok := k.raw(&f); ok {
			values[k.Name] = text
		}
	}
	for _, key := range md.Undecoded() {
		unknown = append(unknown, key.String())
	}
	return values, unknown, nil
}

// InFile reports which settings text sets, by key name and as text. It is
// nil when the text cannot be read as a settings file.
func InFile(text string) map[string]string {
	values, _, err := decode(text)
	if err != nil {
		return nil
	}
	return values
}

// Parse applies TOML text over def. It reports every invalid or unknown key
// together, so a file is fixed in one pass.
func Parse(text string, def Config) (Config, error) {
	values, unknown, err := decode(text)
	if err != nil {
		return def, err
	}

	c := def
	var problems []error
	for _, key := range unknown {
		problems = append(problems, fmt.Errorf("%s: unknown key (known: %s)", key, Names()))
	}
	for _, k := range keys {
		text, ok := values[k.Name]
		if !ok {
			continue
		}
		if err := k.set(&c, text); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", k.Assignment(text), err))
		}
	}
	if len(problems) > 0 {
		return def, errors.Join(problems...)
	}
	return c, nil
}

// parseError keeps the TOML decoder's line number and message without its
// multi-line source excerpt.
func parseError(err error) error {
	if pe, ok := errors.AsType[toml.ParseError](err); ok {
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

// FormatDuration writes d the way the settings file takes it and a person
// would type it: "5m", not "5m0s".
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
