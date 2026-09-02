// Package command runs bounded subprocesses and fully reaps them.
package command

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

const (
	DefaultOutputLimit = 1 << 20
	DefaultTermGrace   = 2 * time.Second
	DefaultKillWait    = 2 * time.Second
)

type Result struct {
	Stdout            []byte
	Stderr            []byte
	StdoutTruncated   bool
	StderrTruncated   bool
	ExitCode          int
	Canceled          bool
	Killed            bool
	CleanupIncomplete bool
}

type Runner interface {
	Run(context.Context, string, []string) (Result, error)
}

type ExecRunner struct {
	StdoutLimit int
	StderrLimit int
	TermGrace   time.Duration
	KillWait    time.Duration
}

type ErrorKind string

const (
	ErrorStart     ErrorKind = "start"
	ErrorExit      ErrorKind = "exit"
	ErrorCanceled  ErrorKind = "canceled"
	ErrorTruncated ErrorKind = "output_truncated"
	ErrorCleanup   ErrorKind = "cleanup_incomplete"
)

type RunError struct {
	Kind  ErrorKind
	Cause error
}

func (e *RunError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("command %s: %v", e.Kind, e.Cause)
	}
	return "command " + string(e.Kind)
}
func (e *RunError) Unwrap() error { return e.Cause }

func (r ExecRunner) Run(ctx context.Context, executable string, args []string) (Result, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return Result{ExitCode: -1, Canceled: true}, &RunError{Kind: ErrorCanceled, Cause: err}
	}
	if executable == "" {
		return Result{ExitCode: -1}, &RunError{Kind: ErrorStart, Cause: errors.New("empty executable")}
	}
	out := newCapBuffer(limit(r.StdoutLimit))
	errOut := newCapBuffer(limit(r.StderrLimit))
	cmd := exec.Command(executable, args...)
	configureProcessGroup(cmd)
	cmd.Stdout, cmd.Stderr = out, errOut
	result := Result{ExitCode: -1}
	if err := cmd.Start(); err != nil {
		return result, &RunError{Kind: ErrorStart, Cause: err}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	leaderReaped := false
	select {
	case waitErr = <-done:
		leaderReaped = true
		// Completion wins only when cancellation was not already observable.
		// This removes scheduler-dependent results when both channels are ready.
		if ctx.Err() != nil {
			result.Canceled = true
			if processGroupExists(cmd) {
				waitErr, result.Killed, result.CleanupIncomplete, leaderReaped = stopProcessGroup(cmd, done, grace(r.TermGrace), killWait(r.KillWait), true, waitErr)
			}
		}
	case <-ctx.Done():
		result.Canceled = true
		select {
		case waitErr = <-done:
			leaderReaped = true
			if processGroupExists(cmd) {
				waitErr, result.Killed, result.CleanupIncomplete, leaderReaped = stopProcessGroup(cmd, done, grace(r.TermGrace), killWait(r.KillWait), true, waitErr)
			}
		default:
			waitErr, result.Killed, result.CleanupIncomplete, leaderReaped = stopProcessGroup(cmd, done, grace(r.TermGrace), killWait(r.KillWait), false, nil)
		}
	}
	result.Stdout, result.StdoutTruncated = out.snapshot()
	result.Stderr, result.StderrTruncated = errOut.snapshot()
	if leaderReaped {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if result.CleanupIncomplete {
		return result, &RunError{Kind: ErrorCleanup, Cause: errors.New("process group still exists after KILL wait")}
	}
	if result.Canceled {
		return result, &RunError{Kind: ErrorCanceled, Cause: ctx.Err()}
	}
	if waitErr != nil {
		return result, &RunError{Kind: ErrorExit, Cause: waitErr}
	}
	if result.StdoutTruncated || result.StderrTruncated {
		return result, &RunError{Kind: ErrorTruncated}
	}
	return result, nil
}

func stopProcessGroup(cmd *exec.Cmd, done <-chan error, termGrace, killGrace time.Duration, leaderDone bool, waitErr error) (error, bool, bool, bool) {
	_ = terminateProcessGroup(cmd)
	timer := time.NewTimer(termGrace)
	if leaderDone {
		done = nil
	}
	for {
		select {
		case waitErr = <-done:
			leaderDone = true
			done = nil
			if !processGroupExists(cmd) {
				if !timer.Stop() {
					<-timer.C
				}
				return waitErr, false, false, true
			}
		case <-timer.C:
			killed := false
			if processGroupExists(cmd) {
				killed = killProcessGroup(cmd) == nil
			}
			waitErr, complete, leaderDone := waitForTeardown(done, killGrace, leaderDone, waitErr, func() bool { return processGroupExists(cmd) })
			return waitErr, killed, !complete, leaderDone
		}
	}
}

func waitForTeardown(done <-chan error, timeout time.Duration, leaderDone bool, waitErr error, groupExists func() bool) (error, bool, bool) {
	if leaderDone {
		done = nil
	}
	if leaderDone && !groupExists() {
		return waitErr, true, true
	}
	timer := time.NewTimer(timeout)
	ticker := time.NewTicker(time.Millisecond)
	defer timer.Stop()
	defer ticker.Stop()
	for {
		select {
		case waitErr = <-done:
			leaderDone = true
			done = nil
			if !groupExists() {
				return waitErr, true, true
			}
		case <-ticker.C:
			if leaderDone && !groupExists() {
				return waitErr, true, true
			}
		case <-timer.C:
			return finishAtDeadline(done, leaderDone, waitErr, groupExists)
		}
	}
}

func finishAtDeadline(done <-chan error, leaderDone bool, waitErr error, groupExists func() bool) (error, bool, bool) {
	if !leaderDone {
		select {
		case waitErr = <-done:
			leaderDone = true
		default:
		}
	}
	return waitErr, leaderDone && !groupExists(), leaderDone
}

func limit(n int) int {
	if n <= 0 {
		return DefaultOutputLimit
	}
	return n
}
func grace(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultTermGrace
	}
	return d
}
func killWait(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultKillWait
	}
	return d
}

type capBuffer struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func newCapBuffer(limit int) *capBuffer { return &capBuffer{limit: limit} }
func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := b.limit - len(b.buf)
	if remaining > 0 {
		if remaining > n {
			remaining = n
		}
		b.buf = append(b.buf, p[:remaining]...)
	}
	if remaining < n {
		b.truncated = true
	}
	return n, nil
}
func (b *capBuffer) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...), b.truncated
}
