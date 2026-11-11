// Package budget places runs under one shared CPU and memory ceiling.
//
// Verification commands are built to saturate a machine: test runners,
// compilers, linters and bundlers each size themselves to the core count, all
// at once, and again for every checkout that runs them concurrently.
// Unbounded, that pins the machine and starves interactive work.
//
// qrun therefore re-executes itself inside a transient systemd scope under
// one user slice, qrun.slice, and puts the limits on the slice. Every run on
// the machine shares that single budget, however many overlap. Inside it the
// kernel schedules fairly, so nothing changes about what runs or in which
// order.
//
// The memory limit is a hard cap with swap off. A soft limit alone fires too
// late: by the time it engages, the rest of the machine has filled RAM and
// reclaim pushes the whole host into swap. The hard cap turns that into a
// kernel kill of one step inside the slice instead.
//
// Enforcement needs a systemd user manager with the cpu controller
// delegated, so it is Linux-only; Available reports whether it can work.
package budget

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/sustinbebustin/qrun/internal/config"
)

// SliceName is the systemd user slice every run joins.
const SliceName = "qrun.slice"

// cpuWeight is below the default of 100 so that, when cores are contended,
// interactive work outside the slice is scheduled ahead of it.
const cpuWeight = 50

// inScopeEnv marks a process already re-executed inside the scope.
const inScopeEnv = "QRUN_IN_SCOPE"

// InScope reports whether this process is already running inside the scope.
func InScope() bool {
	return os.Getenv(inScopeEnv) != ""
}

// Available reports whether a budget can be enforced here: systemd-run and
// systemctl exist, a user manager is reachable, and it has the cpu controller.
func Available() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	// The manager's socket is found by uid, not through XDG_RUNTIME_DIR, for
	// the same reason the queue directory is: harnesses rewrite the variable.
	uid := strconv.Itoa(os.Getuid())
	if _, err := os.Stat("/run/user/" + uid + "/systemd/private"); err != nil {
		return false
	}
	controllers, err := os.ReadFile(
		"/sys/fs/cgroup/user.slice/user-" + uid + ".slice/user@" + uid + ".service/cgroup.controllers")
	if err != nil {
		return false
	}
	return slices.Contains(strings.Fields(string(controllers)), "cpu")
}

// Properties renders b as systemd unit properties for the slice. Every limit
// is always present: the slice outlives any one run, so a limit that is no
// longer wanted has to be reset or its previous value stays in force.
func Properties(b config.Budget) []string {
	quota := "CPUQuota=" // empty resets to no quota
	if b.CPUs > 0 {
		quota = fmt.Sprintf("CPUQuota=%d%%", b.CPUs*100)
	}
	props := []string{quota, fmt.Sprintf("CPUWeight=%d", cpuWeight)}

	if b.MemoryHigh != "" {
		props = append(props, "MemoryHigh="+b.MemoryHigh)
	} else {
		props = append(props, "MemoryHigh=infinity")
	}
	if b.MemoryMax != "" {
		props = append(props, "MemoryMax="+b.MemoryMax, "MemorySwapMax=0")
	} else {
		props = append(props, "MemoryMax=infinity", "MemorySwapMax=infinity")
	}
	return props
}

// Apply sets the slice's limits to b. They are runtime-only, so a reboot
// clears them, and every run reasserts them, so an edit to the settings file
// takes effect on the next run.
func Apply(ctx context.Context, b config.Budget) error {
	args := append([]string{"--user", "set-property", "--runtime", SliceName}, Properties(b)...)
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Command builds the command that runs self with args inside a new scope
// under the slice. The caller wires its stdio and runs it.
//
// OOMPolicy=continue: systemd's default (stop) tears the whole scope down
// when the kernel kills any one process in it, qrun included, so a run would
// end in a bare exit 255 with no summary and no log path. With continue only
// the killed process dies and the step fails normally.
func Command(ctx context.Context, self string, args []string) *exec.Cmd {
	runArgs := append([]string{
		"--user", "--scope", "--quiet",
		"--slice=" + SliceName,
		"-p", "OOMPolicy=continue",
		"--", self,
	}, args...)
	cmd := exec.CommandContext(ctx, "systemd-run", runArgs...)
	cmd.Env = append(os.Environ(), inScopeEnv+"=1")
	return cmd
}
