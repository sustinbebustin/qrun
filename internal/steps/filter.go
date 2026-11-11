package steps

import (
	"regexp"
	"strings"
)

// CSI sequence: ESC [, parameter bytes, intermediate bytes (0x20-0x2f), a
// final byte (0x40-0x7e).
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[\x20-\x2f]*[\x40-\x7e]`)

// stripANSI removes ANSI escape sequences (colors, cursor moves) so captured
// output is plain text whether or not a tool forced color.
func stripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

// salientRE matches the lines that carry the signal in a failure: compiler
// and linter diagnostics (file:line), test failures, panics and TAP
// failures. The patterns are a deliberately broad union across toolchains, so
// a step needs no hint about which tool it runs.
var salientRE = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\.(ts|tsx|js|jsx|mjs|cjs|go|sql|py|rs|rb|java|kt|swift|cs|c|cc|cpp|h|hpp|php):\d+(:\d+)?`), // file:line[:col]
	regexp.MustCompile(`error TS\d+`),                                // tsc
	regexp.MustCompile(`^\s*(FAIL|--- FAIL|✗|×|❯)\b`),                // vitest / go test markers
	regexp.MustCompile(`^FAIL\b`),                                    // go test summary
	regexp.MustCompile(`^panic:`),                                    // go panic
	regexp.MustCompile(`^error(\[E\d+\])?:`),                         // rustc / cargo
	regexp.MustCompile(`(?i)^\s*(Expected|Received|AssertionError)`), // assertion diffs
	regexp.MustCompile(`^not ok\b`),                                  // TAP
	regexp.MustCompile(`^#\s+(Failed|Looks like)`),                   // TAP diagnostics
	regexp.MustCompile(`(?i)\b(Failed to compile|Type error:)`),      // next build
	regexp.MustCompile(`(?i)command (finished with error|failed)`),   // turbo
	regexp.MustCompile(`(?i)^\s*\d+ (error|problem)s?\b`),            // "3 errors" summaries
}

// continuationRE marks indented detail lines worth keeping when they directly
// follow a matched line (the Expected/Received block under a test failure).
var continuationRE = regexp.MustCompile(`^\s+\S`)

const (
	// maxLinesSingle caps the failure excerpt of a single-step run.
	maxLinesSingle = 40
	// maxLinesMulti caps each failing step's excerpt inside a multi-step run,
	// keeping the whole summary bounded.
	maxLinesMulti = 16
)

// extractFailure returns the salient lines of a failed step's output, capped
// at limit. It keeps matched diagnostic lines plus up to two indented detail
// lines that follow each. When nothing matches it falls back to the tail of
// the output, so the excerpt is never empty. elided is the number of salient
// lines dropped by the cap.
func extractFailure(output string, limit int) (lines []string, elided int) {
	raw := strings.Split(strings.TrimRight(output, "\n"), "\n")

	type span struct{ start, end int } // inclusive indices into raw
	var spans []span
	for i := range raw {
		if !salient(raw[i]) {
			continue
		}
		end := i
		for j := i + 1; j <= i+2 && j < len(raw); j++ {
			if !continuationRE.MatchString(raw[j]) || salient(raw[j]) {
				break
			}
			end = j
		}
		spans = append(spans, span{i, end})
	}

	if len(spans) == 0 {
		var tail []string
		for i := len(raw) - 1; i >= 0 && len(tail) < limit; i-- {
			if strings.TrimSpace(raw[i]) != "" {
				tail = append([]string{raw[i]}, tail...)
			}
		}
		return tail, 0
	}

	seen := make(map[int]bool)
	total := 0
	for _, sp := range spans {
		for k := sp.start; k <= sp.end; k++ {
			if seen[k] {
				continue
			}
			seen[k] = true
			total++
			if len(lines) < limit {
				lines = append(lines, strings.TrimRight(raw[k], " \t"))
			}
		}
	}
	return lines, total - len(lines)
}

func salient(line string) bool {
	for _, re := range salientRE {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}
