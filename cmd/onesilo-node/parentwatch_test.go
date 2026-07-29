package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestParentPIDFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    int
		wantSet bool
	}{
		{name: "unset"},
		{name: "valid", env: map[string]string{parentPIDEnvVar: "4242"}, want: 4242, wantSet: true},
		{name: "surrounding whitespace", env: map[string]string{parentPIDEnvVar: " 4242\n"}, want: 4242, wantSet: true},
		{name: "not a number", env: map[string]string{parentPIDEnvVar: "launchd"}},
		{name: "empty", env: map[string]string{parentPIDEnvVar: ""}},
		// init/launchd never exits, so watching it is meaningless.
		{name: "init", env: map[string]string{parentPIDEnvVar: "1"}},
		{name: "negative", env: map[string]string{parentPIDEnvVar: "-3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				v, ok := tc.env[key]
				return v, ok
			}
			got, ok := parentPIDFromEnv(lookup)
			if ok != tc.wantSet || got != tc.want {
				t.Errorf("got (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantSet)
			}
		})
	}
}

func TestParentGone(t *testing.T) {
	cases := []struct {
		name       string
		alive      bool
		trackPPID  bool
		reparented bool
		want       bool
	}{
		{name: "parent alive, still our parent", alive: true, trackPPID: true},
		{name: "parent alive but we were reparented", alive: true, trackPPID: true, reparented: true, want: true},
		{name: "parent exited", trackPPID: true, want: true},
		// Double-forked launcher: our ppid never matched the watched pid,
		// so reparenting says nothing and only liveness counts.
		{name: "not a direct child, parent alive", alive: true, reparented: true},
		{name: "not a direct child, parent exited", reparented: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parentGone(tc.alive, tc.trackPPID, tc.reparented); got != tc.want {
				t.Errorf("parentGone(%v, %v, %v) = %v, want %v",
					tc.alive, tc.trackPPID, tc.reparented, got, tc.want)
			}
		})
	}
}

func TestProcessAliveTracksARealProcess(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("this test process reports as not alive")
	}

	// A reaped child's pid is free again: exactly the case the watcher has
	// to notice.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a helper process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper process: %v", err)
	}
	if processAlive(pid) {
		t.Errorf("pid %d reports as alive after exiting", pid)
	}
}

// The end-to-end shape: a watched parent that exits cancels the context the
// node runs on.
func TestWatchParentCancelsWhenParentExits(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a helper process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	ctx, stop := watchParent(context.Background(), cmd.Process.Pid, slog.New(slog.DiscardHandler))
	defer stop()

	select {
	case <-ctx.Done():
		t.Fatal("context cancelled while the watched process was still running")
	case <-time.After(parentPollInterval + 500*time.Millisecond):
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // reap, so the pid stops resolving

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context not cancelled after the watched process exited")
	}
}

// Stopping the watcher must cancel the derived context, so a normal
// shutdown doesn't leave the poll goroutine running.
func TestWatchParentStopCancels(t *testing.T) {
	ctx, stop := watchParent(context.Background(), os.Getpid(), slog.New(slog.DiscardHandler))
	stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the watch context")
	}
}
