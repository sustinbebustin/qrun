package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The settings file is edited as text, one line at a time, so that the
// comments and layout a person gave it survive `qrun config set`. Every edit
// is then read back: if the file does not now set exactly what was asked and
// nothing else, the edit is refused rather than written. That check is what
// lets the line editing stay simple; a layout it cannot handle (an inline
// table, a dotted key) is caught there, not guessed at.

// ValueError says that a value is not one its key takes.
type ValueError struct {
	Key   Key
	Value string
	Err   error
}

func (e *ValueError) Error() string {
	return fmt.Sprintf("%s: %v", e.Key.Assignment(e.Value), e.Err)
}

func (e *ValueError) Unwrap() error { return e.Err }

// errLayout is returned when an edit did not read back as intended.
var errLayout = errors.New("the key is written in a layout qrun does not edit (an inline table or a dotted key)")

// Set returns the settings file text with key set to value. The value is
// validated and written in its plain form, so "300s" becomes "5m". Text that
// already sets that value comes back unchanged.
func Set(text string, key Key, value string) (string, error) {
	var staged Config
	if err := key.set(&staged, value); err != nil {
		return "", &ValueError{Key: key, Value: value, Err: err}
	}
	value = key.get(staged)

	before, _, err := decode(text)
	if err != nil {
		return "", err
	}
	if was, set := before[key.Name]; set && was == value {
		return text, nil
	}

	lines := splitLines(text)
	table, leaf := key.split()
	assignment := leaf + " = " + key.literal(value)
	start, end, found := region(lines, table)
	if !found {
		lines = appendTable(lines, table, assignment)
	} else if active, placeholder := find(lines[start:end], leaf); active != nil {
		lines[start+active.index] = active.indent + assignment + active.comment
	} else if placeholder != nil {
		lines[start+placeholder.index] = placeholder.indent + assignment
	} else {
		lines = insert(lines, start, end, table == "", assignment)
	}

	want := maps.Clone(before)
	want[key.Name] = value
	return verified(lines, want)
}

// Unset returns the settings file text with key back at its default: its
// line is commented out and shows the default from def. Text that does not
// set the key comes back unchanged.
func Unset(text string, key Key, def Config) (string, error) {
	before, _, err := decode(text)
	if err != nil {
		return "", err
	}
	if _, set := before[key.Name]; !set {
		return text, nil
	}

	lines := splitLines(text)
	table, leaf := key.split()
	start, end, found := region(lines, table)
	if !found {
		return "", errLayout
	}
	active, _ := find(lines[start:end], leaf)
	if active == nil {
		return "", errLayout
	}
	lines[start+active.index] = active.indent + "# " + leaf + " = " + key.literal(key.get(def)) + active.comment

	want := maps.Clone(before)
	delete(want, key.Name)
	return verified(lines, want)
}

// verified joins lines into a file and confirms that it sets exactly want.
func verified(lines []string, want map[string]string) (string, error) {
	text := strings.Join(lines, "\n") + "\n"
	got, _, err := decode(text)
	if err != nil || !maps.Equal(got, want) {
		return "", errLayout
	}
	return text, nil
}

// Template returns a settings file that sets nothing: every key is present
// under its explanation, commented out and showing its default from def.
// Set fills such a file in place, so the explanations stay beside the values.
func Template(def Config) string {
	var b strings.Builder
	b.WriteString("# qrun settings, shared by every run on this machine.\n" +
		"# A line that starts with \"#\" is not in force: it shows the default.\n" +
		"# `qrun config set KEY VALUE` edits this file; `qrun config show` lists the\n" +
		"# values in force.\n")
	table := ""
	for _, k := range keys {
		in, leaf := k.split()
		b.WriteString("\n")
		if in != table {
			table = in
			fmt.Fprintf(&b, "[%s]\n", table)
		}
		for _, line := range k.Doc {
			fmt.Fprintf(&b, "# %s\n", line)
		}
		fmt.Fprintf(&b, "# %s = %s\n", leaf, k.literal(k.get(def)))
	}
	return b.String()
}

// Write replaces the settings file at path with text, creating its directory.
// The file is swapped in whole, so a run that reads it mid-write sees the old
// settings or the new ones, never half of each.
func Write(path, text string) error {
	// A settings file kept in a dotfiles repository is a symlink. The edit
	// goes to its target: replacing the link would detach the file from the
	// repository without saying so.
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("create a file in %s: %w", dir, err)
	}
	if err := replace(tmp, path, text, mode); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name()) //nolint:errcheck // already failing; the temporary file is not the settings file
		return err
	}
	return nil
}

// replace fills tmp and renames it over path.
func replace(tmp *os.File, path, text string, mode os.FileMode) error {
	if _, err := tmp.WriteString(text); err != nil {
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("set the mode of %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// split returns the key's table ("" for the top level) and its name in it.
func (k Key) split() (table, leaf string) {
	if i := strings.LastIndex(k.Name, "."); i >= 0 {
		return k.Name[:i], k.Name[i+1:]
	}
	return "", k.Name
}

func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

var (
	headerRE  = regexp.MustCompile(`^\s*\[`)
	commentRE = regexp.MustCompile(`^\s*#`)
	// valueRE splits what follows "=" into the value and a trailing comment.
	valueRE = regexp.MustCompile(`^(?:"[^"]*"|'[^']*'|[^#]*?)(\s*#.*)?$`)
	// bareValueRE matches one value and nothing after it.
	bareValueRE = regexp.MustCompile(`^(?:"[^"]*"|'[^']*'|[^\s#"']+)\s*$`)
)

// region returns the lines [start, end) that belong to table, where "" is the
// top level. found is false when the file has no such table.
func region(lines []string, table string) (start, end int, found bool) {
	if table != "" {
		header := regexp.MustCompile(`^\s*\[\s*` + regexp.QuoteMeta(table) + `\s*\]\s*(#.*)?$`)
		at := slices.IndexFunc(lines, header.MatchString)
		if at < 0 {
			return 0, 0, false
		}
		start = at + 1
	}
	end = len(lines)
	if next := slices.IndexFunc(lines[start:], headerRE.MatchString); next >= 0 {
		end = start + next
	}
	return start, end, true
}

// keyLine is a line that sets a key, or a commented-out one that would.
type keyLine struct {
	index   int
	indent  string
	comment string // the trailing comment, with the space before it
}

// find looks in lines for the line that sets leaf (active) and, failing
// that, the first commented-out line that would (placeholder). A placeholder
// is a commented-out assignment and nothing more: a comment that goes on to
// say something is a person's note, and is left alone.
func find(lines []string, leaf string) (active, placeholder *keyLine) {
	re := regexp.MustCompile(`^(\s*)(#\s*)?` + regexp.QuoteMeta(leaf) + `\s*=\s*(.*)$`)
	for i, line := range lines {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		found := &keyLine{index: i, indent: m[1]}
		if m[2] != "" {
			if placeholder == nil && bareValueRE.MatchString(m[3]) {
				placeholder = found
			}
			continue
		}
		if value := valueRE.FindStringSubmatch(m[3]); value != nil {
			found.comment = value[1]
		}
		return found, placeholder
	}
	return nil, placeholder
}

// insert adds assignment to the region [start, end) that has no line for its
// key: after the region's last setting or, in a region with none, at its
// head. The top level's head is above the first table and the comment
// attached to it.
func insert(lines []string, start, end int, topLevel bool, assignment string) []string {
	for i := end - 1; i >= start; i-- {
		if strings.TrimSpace(lines[i]) != "" && !commentRE.MatchString(lines[i]) {
			return slices.Insert(lines, i+1, assignment)
		}
	}
	if !topLevel {
		return slices.Insert(lines, start, assignment)
	}

	at := end
	if end < len(lines) {
		for at > 0 && commentRE.MatchString(lines[at-1]) {
			at--
		}
	}
	added := []string{assignment}
	if at > 0 && strings.TrimSpace(lines[at-1]) != "" {
		added = append([]string{""}, added...)
	}
	if at < len(lines) && strings.TrimSpace(lines[at]) != "" {
		added = append(added, "")
	}
	return slices.Insert(lines, at, added...)
}

// appendTable adds a new table holding assignment at the end of the file.
func appendTable(lines []string, table, assignment string) []string {
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
		lines = append(lines, "")
	}
	return append(lines, "["+table+"]", assignment)
}
