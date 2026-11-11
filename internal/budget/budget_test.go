package budget

import (
	"context"
	"slices"
	"testing"

	"github.com/sustinbebustin/qrun/internal/config"
)

func TestProperties(t *testing.T) {
	t.Run("a full budget caps cpu and memory with swap off", func(t *testing.T) {
		got := Properties(config.Budget{Enabled: true, CPUs: 10, MemoryHigh: "8G", MemoryMax: "12G"})
		want := []string{"CPUQuota=1000%", "CPUWeight=50", "MemoryHigh=8G", "MemoryMax=12G", "MemorySwapMax=0"}
		if !slices.Equal(got, want) {
			t.Errorf("props = %v, want %v", got, want)
		}
	})

	// The slice outlives any one run, so a limit that is no longer wanted has
	// to be reset explicitly or the previous value stays in force.
	t.Run("unset limits reset the slice", func(t *testing.T) {
		got := Properties(config.Budget{Enabled: true})
		for _, p := range []string{"CPUQuota=", "MemoryHigh=infinity", "MemoryMax=infinity", "MemorySwapMax=infinity"} {
			if !slices.Contains(got, p) {
				t.Errorf("props = %v, want %s to clear the old limit", got, p)
			}
		}
	})
}

func TestCommandRunsSelfInsideTheSharedSlice(t *testing.T) {
	cmd := Command(context.Background(), "/usr/bin/qrun", []string{"--step", "a=true"})

	want := []string{
		"systemd-run", "--user", "--scope", "--quiet",
		"--slice=" + SliceName,
		"-p", "OOMPolicy=continue",
		"--", "/usr/bin/qrun", "--step", "a=true",
	}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
	if !slices.Contains(cmd.Env, inScopeEnv+"=1") {
		t.Error("the re-executed qrun must be told it is already inside the scope, or it would nest forever")
	}
}

func TestInScope(t *testing.T) {
	t.Setenv(inScopeEnv, "")
	if InScope() {
		t.Error("InScope with the marker unset")
	}
	t.Setenv(inScopeEnv, "1")
	if !InScope() {
		t.Error("!InScope with the marker set")
	}
}
