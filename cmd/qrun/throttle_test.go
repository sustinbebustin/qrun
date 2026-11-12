package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestLimited(t *testing.T) {
	// hung is work that runs until it is stopped.
	hung := func(ctx context.Context) int {
		<-ctx.Done()
		return exitInterrupted
	}

	t.Run("stops work that outlasts the limit and says how to raise it", func(t *testing.T) {
		var stderr bytes.Buffer
		code := limited(context.Background(), &stderr, 20*time.Millisecond, hung)
		if code != exitTimedOut {
			t.Errorf("exit %d, want %d", code, exitTimedOut)
		}
		for _, want := range []string{"time limit of 20ms", "qrun config set time_limit"} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
			}
		}
	})

	t.Run("gives the work the reason for its summary", func(t *testing.T) {
		var cause error
		limited(context.Background(), &bytes.Buffer{}, 20*time.Millisecond, func(ctx context.Context) int {
			<-ctx.Done()
			cause = context.Cause(ctx)
			return exitInterrupted
		})
		if cause == nil || cause.Error() != "timed out at the 20ms time limit" {
			t.Errorf("cause = %v", cause)
		}
	})

	t.Run("leaves work that finishes in time with its own result", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := limited(context.Background(), &stderr, time.Minute, func(context.Context) int { return 7 }); code != 7 {
			t.Errorf("exit %d, want the work's own 7", code)
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want nothing", stderr.String())
		}
	})

	// The deadline can pass in the instant after the work finishes.
	t.Run("work that passed is not failed by a deadline that came after it", func(t *testing.T) {
		var stderr bytes.Buffer
		code := limited(context.Background(), &stderr, 20*time.Millisecond, func(ctx context.Context) int {
			<-ctx.Done()
			return exitOK
		})
		if code != exitOK || stderr.Len() != 0 {
			t.Errorf("exit %d, stderr %q, want a clean pass", code, stderr.String())
		}
	})

	t.Run("no limit sets no deadline", func(t *testing.T) {
		limited(context.Background(), &bytes.Buffer{}, 0, func(ctx context.Context) int {
			if deadline, ok := ctx.Deadline(); ok {
				t.Errorf("work got a deadline of %v with no limit set", deadline)
			}
			return exitOK
		})
	})

	t.Run("an interrupt under a limit is still an interrupt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stderr bytes.Buffer
		if code := limited(ctx, &stderr, time.Minute, hung); code != exitInterrupted || stderr.Len() != 0 {
			t.Errorf("exit %d, stderr %q, want a plain interrupt", code, stderr.String())
		}
	})
}
