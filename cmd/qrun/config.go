package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/sustinbebustin/qrun/internal/config"
)

// settingsFile is the machine-wide settings file as it is on disk now.
type settingsFile struct {
	path   string
	text   string
	exists bool
	// def is this machine's defaults, which the file's values go over.
	def config.Config
}

func readSettingsFile() (settingsFile, error) {
	sf := settingsFile{path: config.Path(), def: config.Default(config.ThisMachine())}
	data, err := os.ReadFile(sf.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return sf, err
	default:
		sf.text, sf.exists = string(data), true
	}
	return sf, nil
}

// cmdConfig reads and edits the settings file. Every verb but show, get,
// check and path changes the file; none of them changes a run directly.
func cmdConfig(argv []string, stdout, stderr io.Writer) int {
	verb, rest := "show", argv
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		verb, rest = argv[0], argv[1:]
	}
	for _, arg := range rest {
		if arg == "--help" || arg == "-h" {
			fmt.Fprint(stdout, helpConfig())
			return exitOK
		}
	}
	if verb == "help" {
		fmt.Fprint(stdout, helpConfig())
		return exitOK
	}
	if verb == "path" {
		if len(rest) > 0 {
			return configUsage(stderr, "path takes no arguments", "qrun config path")
		}
		fmt.Fprintln(stdout, config.Path())
		return exitOK
	}

	sf, err := readSettingsFile()
	if err != nil {
		fmt.Fprintf(stderr, "qrun: cannot read the settings file: %v\n  Nothing was changed. Runs use the defaults until it can be read; check its permissions.\n", err)
		return exitFailed
	}
	switch verb {
	case "show":
		return configShow(sf, rest, stdout, stderr)
	case "get":
		return configGet(sf, rest, stdout, stderr)
	case "set":
		return configSet(sf, rest, stdout, stderr)
	case "unset":
		return configUnset(sf, rest, stdout, stderr)
	case "init":
		return configInit(sf, rest, stdout, stderr)
	case "check":
		return configCheck(sf, rest, stdout, stderr)
	default:
		return configUsage(stderr, fmt.Sprintf("unknown config command %q (known: show, get, set, unset, init, check, path)", verb), "qrun config set slots 2")
	}
}

func configUsage(stderr io.Writer, problem, example string) int {
	fmt.Fprint(stderr, usageError(problem, example, "qrun config --help"))
	return exitUsage
}

// splitFlags separates the flags a config verb allows from its other
// arguments. A lone "-" or a negative number is an argument, not a flag.
func splitFlags(argv []string, allowed ...string) (flags map[string]bool, args []string, err error) {
	flags = make(map[string]bool)
	for _, arg := range argv {
		switch {
		case !strings.HasPrefix(arg, "--"):
			args = append(args, arg)
		case slices.Contains(allowed, arg):
			flags[arg] = true
		default:
			return nil, nil, fmt.Errorf("unknown flag %q", arg)
		}
	}
	return flags, args, nil
}

// lookupKey finds a setting by name, or says which names exist.
func lookupKey(name string) (config.Key, error) {
	key, ok := config.Lookup(name)
	if !ok {
		return config.Key{}, fmt.Errorf("unknown setting %q (known: %s)", name, config.Names())
	}
	return key, nil
}

// warnIgnored says on stderr that the file is invalid, so what was just
// printed is the defaults.
func warnIgnored(stderr io.Writer, problem error) {
	if problem != nil {
		fmt.Fprintf(stderr, "qrun: the settings file is invalid, so these are the defaults:\n%s\n", indent(problem.Error()))
	}
}

// configShowReport is the --json form of `qrun config show`.
type configShowReport struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	// Error is set when the file exists but was rejected, so every value is
	// its default.
	Error    string          `json:"error,omitempty"`
	Settings []settingReport `json:"settings"`
}

// settingReport is one setting in force. Values are in the text form
// `qrun config set` takes.
type settingReport struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Default string `json:"default"`
	// Source is "file" when the settings file sets the value, else "default".
	Source string `json:"source"`
}

// configShow prints every setting in force and where it comes from. The text
// form is itself a valid settings file.
func configShow(sf settingsFile, argv []string, stdout, stderr io.Writer) int {
	flags, args, err := splitFlags(argv, "--json")
	if err == nil && len(args) > 0 {
		err = fmt.Errorf("unexpected argument %q to show", args[0])
	}
	if err != nil {
		return configUsage(stderr, err.Error(), "qrun config show --json")
	}

	cfg, problem := config.Parse(sf.text, sf.def)
	inFile := map[string]string{}
	if problem == nil {
		inFile = config.InFile(sf.text)
	}
	report := configShowReport{Path: sf.path, Exists: sf.exists}
	if problem != nil {
		report.Error = problem.Error()
	}
	for _, key := range config.Keys() {
		setting := settingReport{Key: key.Name, Value: key.Value(cfg), Default: key.Value(sf.def), Source: "default"}
		if _, set := inFile[key.Name]; set {
			setting.Source = "file"
		}
		report.Settings = append(report.Settings, setting)
	}

	if flags["--json"] {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "qrun: cannot write the settings: %v\n", err)
			return exitFailed
		}
		return exitOK
	}

	width := 0
	for _, key := range config.Keys() {
		width = max(width, len(key.Assignment(key.Value(cfg))))
	}
	for i, key := range config.Keys() {
		source := "default"
		if report.Settings[i].Source == "file" {
			source = fmt.Sprintf("set in the settings file (default: %s)", report.Settings[i].Default)
		}
		fmt.Fprintf(stdout, "%-*s  # %s\n", width, key.Assignment(key.Value(cfg)), source)
	}
	switch {
	case problem != nil:
		fmt.Fprintf(stdout, "# settings file: %s (invalid, so the defaults are in force)\n", sf.path)
	case sf.exists:
		fmt.Fprintf(stdout, "# settings file: %s\n", sf.path)
	default:
		fmt.Fprintf(stdout, "# settings file: %s (not created; `qrun config set KEY VALUE` creates it)\n", sf.path)
	}
	warnIgnored(stderr, problem)
	return exitOK
}

// configGet prints one value in force, bare, for a script to capture.
func configGet(sf settingsFile, argv []string, stdout, stderr io.Writer) int {
	_, args, err := splitFlags(argv)
	if err == nil && len(args) != 1 {
		err = errors.New("get takes exactly one KEY")
	}
	var key config.Key
	if err == nil {
		key, err = lookupKey(args[0])
	}
	if err != nil {
		return configUsage(stderr, err.Error(), "qrun config get slots")
	}
	cfg, problem := config.Parse(sf.text, sf.def)
	fmt.Fprintln(stdout, key.Value(cfg))
	warnIgnored(stderr, problem)
	return exitOK
}

// assignment is one KEY VALUE pair given to `qrun config set`.
type assignment struct {
	key   config.Key
	value string
}

// parseAssignments reads `KEY VALUE`, or one or more `KEY=VALUE`.
func parseAssignments(args []string) ([]assignment, error) {
	pairs := make([][2]string, 0, len(args))
	if len(args) == 2 && !strings.Contains(args[0], "=") {
		pairs = append(pairs, [2]string{args[0], args[1]})
	} else {
		for _, arg := range args {
			name, value, ok := strings.Cut(arg, "=")
			if !ok {
				return nil, fmt.Errorf("set takes KEY VALUE, or one or more KEY=VALUE; got %q", arg)
			}
			pairs = append(pairs, [2]string{name, value})
		}
	}
	if len(pairs) == 0 {
		return nil, errors.New("nothing to set")
	}
	assignments := make([]assignment, len(pairs))
	for i, pair := range pairs {
		key, err := lookupKey(pair[0])
		if err != nil {
			return nil, err
		}
		assignments[i] = assignment{key: key, value: pair[1]}
	}
	return assignments, nil
}

// configSet changes settings by editing the file. A file that does not exist
// yet starts as the commented template, so it explains itself from the start.
func configSet(sf settingsFile, argv []string, stdout, stderr io.Writer) int {
	flags, args, err := splitFlags(argv, "--dry-run")
	var assignments []assignment
	if err == nil {
		assignments, err = parseAssignments(args)
	}
	if err != nil {
		return configUsage(stderr, err.Error(), "qrun config set slots 2")
	}

	before, _ := config.Parse(sf.text, sf.def) //nolint:errcheck // an invalid file is in force as the defaults, which Parse returns
	text := sf.text
	if !sf.exists {
		text = config.Template(sf.def)
	}
	lines := make([]string, len(assignments))
	for i, a := range assignments {
		text, err = config.Set(text, a.key, a.value)
		if valueErr, ok := errors.AsType[*config.ValueError](err); ok {
			return configUsage(stderr, valueErr.Error(), "qrun config set "+a.key.Name+" "+a.key.Example)
		}
		if err != nil {
			return cannotEdit(stderr, sf.path, err)
		}
		lines[i] = changeLine(a.key, before, text, sf.def)
	}
	return finishEdit(sf, text, lines, flags["--dry-run"], stdout, stderr)
}

// configUnset puts settings back to their defaults by editing the file.
func configUnset(sf settingsFile, argv []string, stdout, stderr io.Writer) int {
	flags, args, err := splitFlags(argv, "--dry-run")
	if err == nil && len(args) == 0 {
		err = errors.New("nothing to unset")
	}
	keys := make([]config.Key, len(args))
	for i := 0; err == nil && i < len(args); i++ {
		keys[i], err = lookupKey(args[i])
	}
	if err != nil {
		return configUsage(stderr, err.Error(), "qrun config unset slots")
	}

	before, _ := config.Parse(sf.text, sf.def) //nolint:errcheck // an invalid file is in force as the defaults, which Parse returns
	text := sf.text
	lines := make([]string, len(keys))
	for i, key := range keys {
		if text, err = config.Unset(text, key, sf.def); err != nil {
			return cannotEdit(stderr, sf.path, err)
		}
		lines[i] = changeLine(key, before, text, sf.def)
	}
	return finishEdit(sf, text, lines, flags["--dry-run"], stdout, stderr)
}

// changeLine reports one key after an edit: its new value and what it was.
func changeLine(key config.Key, before config.Config, text string, def config.Config) string {
	// An edit that leaves other problems in the file reads back as the
	// defaults; the value just written is still what the file now says.
	now := key.Value(def)
	if written, set := config.InFile(text)[key.Name]; set {
		now = written
	}
	if was := key.Value(before); was != now {
		return fmt.Sprintf("%s  (was %s)", key.Assignment(now), was)
	}
	return fmt.Sprintf("%s  (unchanged)", key.Assignment(now))
}

func cannotEdit(stderr io.Writer, path string, err error) int {
	fmt.Fprintf(stderr, "qrun: cannot edit %s: %v\n  Nothing was changed. Edit the file by hand; `qrun config check` validates it.\n", path, err)
	return exitFailed
}

// finishEdit reports an edit and writes the file if it changed. It fails
// when the file is still invalid afterwards, since the defaults then stay in
// force whatever was just written.
func finishEdit(sf settingsFile, text string, lines []string, dryRun bool, stdout, stderr io.Writer) int {
	outcome := fmt.Sprintf("settings file: %s (in force from the next run)", sf.path)
	switch {
	case text == sf.text:
		outcome = fmt.Sprintf("settings file: %s (not changed)", sf.path)
	case dryRun:
		outcome = fmt.Sprintf("dry run: %s not written", sf.path)
	default:
		// Written before anything is reported, so a failed write is not
		// preceded by lines that say what changed.
		if err := config.Write(sf.path, text); err != nil {
			fmt.Fprintf(stderr, "qrun: cannot write the settings file: %v\n  Nothing was changed.\n", err)
			return exitFailed
		}
	}
	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintln(stdout, outcome)
	if _, problem := config.Parse(text, sf.def); problem != nil {
		fmt.Fprintf(stderr, "qrun: the settings file has other problems, so the defaults stay in force until they are fixed:\n%s\n", indent(problem.Error()))
		return exitFailed
	}
	return exitOK
}

// configInit creates the settings file as the commented template. An existing
// file is left alone, so running it twice is safe.
func configInit(sf settingsFile, argv []string, stdout, stderr io.Writer) int {
	flags, args, err := splitFlags(argv, "--dry-run")
	if err == nil && len(args) > 0 {
		err = fmt.Errorf("unexpected argument %q to init", args[0])
	}
	if err != nil {
		return configUsage(stderr, err.Error(), "qrun config init")
	}
	switch {
	case sf.exists:
		fmt.Fprintf(stdout, "already exists: %s (left as it is)\n", sf.path)
	case flags["--dry-run"]:
		fmt.Fprint(stdout, config.Template(sf.def))
		fmt.Fprintf(stderr, "qrun: dry run: %s not written\n", sf.path)
	default:
		if err := config.Write(sf.path, config.Template(sf.def)); err != nil {
			fmt.Fprintf(stderr, "qrun: cannot write the settings file: %v\n  Nothing was changed.\n", err)
			return exitFailed
		}
		fmt.Fprintf(stdout, "created: %s (every key commented out at its default)\n", sf.path)
	}
	return exitOK
}

// configCheck validates the settings file, or another file given by path.
func configCheck(sf settingsFile, argv []string, stdout, stderr io.Writer) int {
	_, args, err := splitFlags(argv)
	if err == nil && len(args) > 1 {
		err = errors.New("check takes at most one FILE")
	}
	if err != nil {
		return configUsage(stderr, err.Error(), "qrun config check")
	}

	path, text := sf.path, sf.text
	if len(args) == 1 {
		data, err := os.ReadFile(args[0])
		if err != nil {
			fmt.Fprintf(stderr, "qrun: cannot read %s: %v\n", args[0], err)
			return exitFailed
		}
		path, text = args[0], string(data)
	} else if !sf.exists {
		fmt.Fprintf(stdout, "ok: %s does not exist, so the defaults are in force\n", sf.path)
		return exitOK
	}

	if _, problem := config.Parse(text, sf.def); problem != nil {
		fmt.Fprintf(stderr, "qrun: %s is not a valid settings file:\n%s\n  Runs ignore an invalid file and use the defaults. `qrun config --help` lists the keys and their values.\n", path, indent(problem.Error()))
		return exitFailed
	}
	fmt.Fprintf(stdout, "ok: %s\n", path)
	return exitOK
}

// helpConfig is `qrun config --help`. The keys come from the settings
// themselves, so the help cannot fall behind them.
func helpConfig() string {
	var keys strings.Builder
	for _, key := range config.Keys() {
		fmt.Fprintf(&keys, "  %s\n", key.Name)
		for _, line := range key.Doc {
			fmt.Fprintf(&keys, "      %s\n", line)
		}
	}
	return `qrun config - read or change the machine-wide settings

Usage:
  qrun config [show] [--json]        the values in force and where each comes from
  qrun config get KEY                one value, bare
  qrun config set KEY VALUE          change a setting; or KEY=VALUE ... for several
  qrun config unset KEY [KEY ...]    put settings back to their defaults
  qrun config init                   create the settings file, every key commented
                                     out at its default
  qrun config check [FILE]           validate the settings file, or FILE
  qrun config path                   print where the settings file is

Settings are per machine: every run shares one line and one budget, so there
is one settings file and no per-run override. set and unset edit that file in
place and keep its comments. A change is in force from the next run; a new
slot count also reaches runs that are already waiting.

Keys:
` + keys.String() + `
Flags:
  --json      show: machine-readable output
  --dry-run   set, unset, init: report what would change and write nothing

Examples:
  qrun config
  qrun config set slots 2
  qrun config set budget.cpus=8 budget.memory_max=16G
  qrun config set --dry-run hold 10m
  qrun config unset slots
  qrun config get budget.memory_max

Exit status: 0; 1 if the settings file is invalid or cannot be written; 2 usage
error. An invalid file never blocks a run: it is ignored and the defaults apply.
`
}
