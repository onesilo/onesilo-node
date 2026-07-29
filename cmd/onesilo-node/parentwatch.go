package main

// Parent-death supervision.
//
// The Silo Mac app runs onesilo-node as a child process and SIGTERMs it on
// quit. A hard kill of the app — a crash, or Xcode's stop button — never
// runs that teardown, and the orphaned node keeps holding 127.0.0.1:8766.
// The next launch then can't bind the admin port and crash-loops until the
// orphan happens to notice its stdout pipe is gone. Setting
// SILO_NODE_PARENT_PID makes the node exit with its parent instead, so the
// port is free by the time the app comes back.
//
// macOS has no PR_SET_PDEATHSIG, so the parent is polled: a signal-0 probe
// of the recorded pid fails once it is really gone, and an orphaned child
// is reparented (getppid changes, to launchd on macOS). Either signal
// cancels the run context, which is the same graceful shutdown path
// SIGTERM takes — capabilities stopped, destination deregistered, admin
// port released.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// parentPIDEnvVar names the process to exit with. Unset (the Docker and
// headless case) means no parent supervision at all.
const parentPIDEnvVar = "SILO_NODE_PARENT_PID"

// parentPollInterval paces the liveness probe. A second is far below the
// human-visible restart latency it protects and costs nothing.
const parentPollInterval = time.Second

// parentPIDFromEnv reads the supervised parent pid. A malformed or
// nonsensical value (pid 1 is init/launchd, which never goes away) is
// treated as "not set" rather than as a startup error: parent supervision
// is an optimization, not a requirement.
func parentPIDFromEnv(lookup func(string) (string, bool)) (int, bool) {
	raw, ok := lookup(parentPIDEnvVar)
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || pid <= 1 {
		return 0, false
	}
	return pid, true
}

// processAlive reports whether pid still exists. EPERM means it exists and
// belongs to someone else — alive for our purposes.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// parentGone decides whether the watched parent has died.
//
// reparented is only consulted when we started out as a direct child
// (trackPPID): a launcher that double-forks hands us an unrelated ppid from
// the very first tick, and treating that as bereavement would exit a node
// whose parent is alive and well.
func parentGone(alive, trackPPID, reparented bool) bool {
	if trackPPID && reparented {
		return true
	}
	return !alive
}

// watchParent returns a context cancelled when pid exits, plus a stop
// function that tears the watcher down.
func watchParent(ctx context.Context, pid int, logger *slog.Logger) (context.Context, context.CancelFunc) {
	watched, cancel := context.WithCancel(ctx)
	trackPPID := os.Getppid() == pid

	go func() {
		ticker := time.NewTicker(parentPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-watched.Done():
				return
			case <-ticker.C:
				if parentGone(processAlive(pid), trackPPID, os.Getppid() != pid) {
					logger.Info("parent process exited, shutting down", "parent_pid", pid)
					cancel()
					return
				}
			}
		}
	}()

	return watched, cancel
}
