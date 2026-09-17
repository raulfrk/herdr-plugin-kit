package agenthost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

const (
	SupportedOpenCodeVersion                  = "1.18.30"
	openCodePromptServerTimeout time.Duration = 30 * time.Second
	openCodePromptServerPoll    time.Duration = 50 * time.Millisecond
	openCodePromptHealthTimeout time.Duration = time.Second
	openCodePromptServerStop    time.Duration = 5 * time.Second
)

type openCodePromptServerLauncher interface {
	Start(context.Context, string, string) (OpenCodeServer, func(context.Context) error, error)
}

type nativeOpenCodePromptServerLauncher struct {
	runner  command.Runner
	reserve func() (int, error)
	health  func(context.Context, string) error
	wait    func(context.Context, time.Duration) error
	probe   time.Duration
}

type openCodeServerRun struct {
	result command.Result
	err    error
}

const openCodeOwnershipOutputLimit = 4096

type openCodeOwnershipObserver struct {
	mu       sync.Mutex
	wantLine []byte
	output   []byte
	matched  bool
}

func newOpenCodeOwnershipObserver(url string) *openCodeOwnershipObserver {
	return &openCodeOwnershipObserver{wantLine: []byte("opencode server listening on " + url + "\n")}
}

func (observer *openCodeOwnershipObserver) Write(p []byte) (int, error) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.matched {
		return len(p), nil
	}
	remaining := openCodeOwnershipOutputLimit - len(observer.output)
	if remaining > 0 {
		observer.output = append(observer.output, p[:min(len(p), remaining)]...)
		observer.matched = bytes.Contains(observer.output, observer.wantLine)
	}
	return len(p), nil
}

func (observer *openCodeOwnershipObserver) ready() bool {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.matched
}

func (launcher nativeOpenCodePromptServerLauncher) Start(parent context.Context, executable, directory string) (OpenCodeServer, func(context.Context) error, error) {
	if parent == nil {
		panic("nil context")
	}
	if launcher.runner == nil || executable == "" || filepath.Base(executable) != "opencode" ||
		strings.ContainsAny(executable, "\x00\r\n") || filepath.Base(executable) != executable && (!filepath.IsAbs(executable) || filepath.Clean(executable) != executable) {
		return OpenCodeServer{}, nil, errors.New("OpenCode prompt server executable is invalid")
	}
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || strings.ContainsRune(directory, 0) {
		return OpenCodeServer{}, nil, errors.New("OpenCode prompt server directory is invalid")
	}
	reserve := launcher.reserve
	if reserve == nil {
		reserve = reserveOpenCodePromptPort
	}
	port, err := reserve()
	if err != nil || port < 1 || port > 65535 {
		return OpenCodeServer{}, nil, errors.New("reserve OpenCode prompt server port failed")
	}
	server := OpenCodeServer{URL: "http://127.0.0.1:" + strconv.Itoa(port), Directory: directory}
	specRunner, ok := launcher.runner.(command.SpecRunner)
	if !ok {
		return OpenCodeServer{}, nil, errors.New("OpenCode prompt server runner does not support observed startup")
	}
	ownership := newOpenCodeOwnershipObserver(server.URL)
	runCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	done := make(chan openCodeServerRun, 1)
	go func() {
		result, runErr := specRunner.RunSpec(runCtx, command.Spec{
			Executable:     executable,
			Args:           []string{"serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port)},
			Dir:            directory,
			StdoutObserver: ownership,
		})
		done <- openCodeServerRun{result: result, err: runErr}
	}()
	var once sync.Once
	stop := func(ctx context.Context) error {
		var stopErr error
		once.Do(func() {
			cancel()
			select {
			case run := <-done:
				if run.result.CleanupIncomplete || run.err != nil && !run.result.Canceled {
					stopErr = errors.New("OpenCode prompt server cleanup failed")
				}
			case <-ctx.Done():
				stopErr = errors.Join(errors.New("OpenCode prompt server cleanup timed out"), ctx.Err())
			}
		})
		return stopErr
	}
	fail := func(cause error) (OpenCodeServer, func(context.Context) error, error) {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(parent), openCodePromptServerStop)
		defer cleanupCancel()
		return OpenCodeServer{}, nil, errors.Join(cause, stop(cleanupCtx))
	}
	health := launcher.health
	if health == nil {
		health = readOpenCodePromptServerHealth
	}
	wait := launcher.wait
	if wait == nil {
		wait = waitManaged
	}
	readyCtx, readyCancel := context.WithTimeout(parent, openCodePromptServerTimeout)
	defer readyCancel()
	probeTimeout := launcher.probe
	if probeTimeout <= 0 {
		probeTimeout = openCodePromptHealthTimeout
	}
	var firstHealthErr error
	for {
		select {
		case run := <-done:
			cancel()
			return OpenCodeServer{}, nil, errors.Join(errors.New("OpenCode prompt server exited before readiness"), run.err)
		default:
		}
		if !ownership.ready() {
			if err := wait(readyCtx, openCodePromptServerPoll); err != nil {
				return fail(errors.Join(errors.New("OpenCode prompt server ownership proof failed"), err))
			}
			continue
		}
		probeCtx, probeCancel := context.WithTimeout(readyCtx, probeTimeout)
		err := health(probeCtx, server.URL)
		probeCancel()
		if err == nil {
			return server, stop, nil
		}
		if firstHealthErr == nil {
			firstHealthErr = err
		}
		if err := wait(readyCtx, openCodePromptServerPoll); err != nil {
			return fail(errors.Join(errors.New("OpenCode prompt server readiness failed"), firstHealthErr, err))
		}
	}
}

func reserveOpenCodePromptPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	return port, listener.Close()
}

func readOpenCodePromptServerHealth(ctx context.Context, base string) error {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	status, body, err := openCodeGET(ctx, client, base+"/global/health")
	if err != nil {
		return fmt.Errorf("OpenCode prompt server health request failed: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("OpenCode prompt server health returned status %d", status)
	}
	var health struct {
		Healthy bool   `json:"healthy"`
		Version string `json:"version"`
	}
	if err := decodeOpenCode(body, &health); err != nil || !health.Healthy || health.Version != SupportedOpenCodeVersion {
		return fmt.Errorf("OpenCode prompt server health is incompatible: healthy=%t version=%q", health.Healthy, health.Version)
	}
	return nil
}
