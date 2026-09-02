//go:build linux

package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"pgregory.net/rapid"
)

type outcome struct {
	result Result
	err    error
}

func TestRunnerBoundsOutputAndReportsTruncation(t *testing.T) {
	result, err := (ExecRunner{StdoutLimit: 4, StderrLimit: 3}).Run(context.Background(), "sh", []string{"-c", "printf abcdef; printf 12345 >&2"})
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Kind != ErrorTruncated {
		t.Fatalf("error = %v", err)
	}
	if string(result.Stdout) != "abcd" || string(result.Stderr) != "123" || !result.StdoutTruncated || !result.StderrTruncated {
		t.Fatalf("result = %+v", result)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
}

func TestRunnerReturnsDeterministicExit(t *testing.T) {
	result, err := (ExecRunner{}).Run(context.Background(), "sh", []string{"-c", "exit 7"})
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Kind != ErrorExit {
		t.Fatalf("error = %v", err)
	}
	if result.ExitCode != 7 || result.Canceled || result.Killed {
		t.Fatalf("result = %+v", result)
	}
}

func TestPropertyBoundedBuffer(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		limit := rapid.IntRange(1, 256).Draw(t, "limit")
		length := rapid.IntRange(0, 512).Draw(t, "length")
		buffer := newCapBuffer(limit)
		input := []byte(strings.Repeat("x", length))
		written, err := buffer.Write(input)
		if err != nil || written != length {
			t.Fatalf("write=(%d,%v), length=%d", written, err, length)
		}
		got, truncated := buffer.snapshot()
		wantLength := length
		if wantLength > limit {
			wantLength = limit
		}
		if len(got) != wantLength || truncated != (length > limit) {
			t.Fatalf("limit=%d length=%d got=%d truncated=%v", limit, length, len(got), truncated)
		}
	})
}

func TestRunnerCancellationKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		result, err := (ExecRunner{TermGrace: 30 * time.Millisecond}).Run(ctx, "sh", []string{"-c", `trap '' TERM; sh -c 'trap "" TERM; echo $$ > "$1"; sleep 30' child "$1"`, "runner-test", pidFile})
		done <- outcome{result: result, err: err}
	}()
	pidBytes := waitForPIDFile(t, pidFile)
	cancel()
	got := waitOutcome(t, done)
	result, err := got.result, got.err
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Kind != ErrorCanceled {
		t.Fatalf("error = %v", err)
	}
	if !result.Canceled || !result.Killed {
		t.Fatalf("result = %+v", result)
	}
	assertProcessGone(t, pidBytes)
}

func TestRunnerKillsDescendantAfterLeaderExitsOnTerm(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		result, err := (ExecRunner{TermGrace: 30 * time.Millisecond}).Run(ctx, "sh", []string{"-c", `sh -c 'trap "" TERM; echo $$ > "$1"; exec sleep 30' child "$1" </dev/null >/dev/null 2>&1 & wait`, "runner-test", pidFile})
		done <- outcome{result, err}
	}()
	pidBytes := waitForPIDFile(t, pidFile)
	cancel()
	got := waitOutcome(t, done)
	var runErr *RunError
	if !errors.As(got.err, &runErr) || runErr.Kind != ErrorCanceled || !got.result.Killed {
		t.Fatalf("result=%+v error=%v", got.result, got.err)
	}
	assertProcessGone(t, pidBytes)
}

func TestTeardownDeadlineDoesNotRequireLeaderCompletion(t *testing.T) {
	done := make(chan error)
	started := time.Now()
	_, complete, leaderReaped := waitForTeardown(done, 10*time.Millisecond, false, nil, func() bool { return true })
	if complete || leaderReaped {
		t.Fatalf("complete=%v leaderReaped=%v", complete, leaderReaped)
	}
	if elapsed := time.Since(started); elapsed < 5*time.Millisecond || elapsed > time.Second {
		t.Fatalf("teardown deadline elapsed in %v", elapsed)
	}
}

func TestTeardownPrefersObservableLeaderCompletionAtDeadline(t *testing.T) {
	wantErr := errors.New("leader exited")
	for i := 0; i < 100; i++ {
		iterationDone := make(chan error, 1)
		iterationDone <- wantErr
		gotErr, complete, leaderReaped := finishAtDeadline(iterationDone, false, nil, func() bool { return false })
		if !errors.Is(gotErr, wantErr) || !complete || !leaderReaped {
			t.Fatalf("iteration=%d error=%v complete=%v leaderReaped=%v", i, gotErr, complete, leaderReaped)
		}
	}
}

func waitForPIDFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		if len(data) != 0 {
			return data
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("child did not become ready")
	return nil
}

func assertProcessGone(t *testing.T, pidBytes []byte) {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("child pid output %q: %v", pidBytes, err)
	}
	if processExists(pid) {
		t.Fatalf("descendant process %d existed after Run returned", pid)
	}
}

func waitOutcome(t *testing.T, done <-chan outcome) outcome {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not complete within teardown bound")
		return outcome{}
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
