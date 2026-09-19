//go:build linux

package servicehost

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestServerClientStatusLifecycle(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", "service.sock")
	server, stop := startTestServer(t, ServerConfig{
		SocketPath:       socketPath,
		ProtocolVersion:  1,
		ServiceName:      "test-service",
		ServiceVersion:   "1.2.3",
		InitialState:     HealthReady,
		InitialAccepting: true,
		InitialDetail:    "ready",
	})
	defer stop()

	client := dialTestClient(t, socketPath, 1)
	handshake := client.Handshake()
	if handshake.ProtocolVersion != 1 || handshake.ServiceName != "test-service" || handshake.ServiceVersion != "1.2.3" {
		t.Fatalf("unexpected handshake: %+v", handshake)
	}
	assertMode(t, filepath.Dir(socketPath), 0o700)
	assertMode(t, socketPath, 0o600)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	health, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.State != HealthReady || !health.Accepting || health.Detail != "ready" || health.Revision != 1 {
		t.Fatalf("unexpected health: %+v", health)
	}

	resetWatcher, err := client.Watch(ctx, Cursor{})
	if err != nil {
		t.Fatalf("watch from empty cursor: %v", err)
	}
	reset, err := resetWatcher.Recv()
	if err != nil {
		t.Fatalf("receive reset: %v", err)
	}
	if !reset.Reset || reset.Status != health {
		t.Fatalf("unexpected reset update: %+v", reset)
	}

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	watcher, err := client.Watch(watchCtx, Cursor{Epoch: health.Epoch, Revision: health.Revision})
	if err != nil {
		t.Fatalf("watch current cursor: %v", err)
	}
	waitForWatchers(t, server.hub, 2)
	received := make(chan StatusUpdate, 1)
	receiveErr := make(chan error, 1)
	go func() {
		update, err := watcher.Recv()
		if err != nil {
			receiveErr <- err
			return
		}
		received <- update
	}()
	updated, err := server.UpdateStatus(HealthDegraded, false, "paused")
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	select {
	case update := <-received:
		if update.Reset || update.Status != updated {
			t.Fatalf("unexpected live update: %+v", update)
		}
	case err := <-receiveErr:
		t.Fatalf("receive live update: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for live update")
	}
	cancelWatch()

	if err := client.Close(); err != nil {
		t.Fatalf("close first client: %v", err)
	}
	second := dialTestClient(t, socketPath, 1)
	if _, err := second.Health(ctx); err != nil {
		t.Fatalf("service stopped with first client: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second client: %v", err)
	}
}

func TestServerRejectsIncompatibleProtocol(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", "service.sock")
	_, stop := startTestServer(t, ServerConfig{
		SocketPath:      socketPath,
		ProtocolVersion: 1,
		ServiceName:     "test-service",
		ServiceVersion:  "1.0.0",
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Dial(ctx, ClientConfig{SocketPath: socketPath, ProtocolVersion: 2, ClientName: "test-client"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("wrong protocol error = %v, want FailedPrecondition", err)
	}

	compatible := dialTestClient(t, socketPath, 1)
	defer compatible.Close()
	if _, err := compatible.Health(ctx); err != nil {
		t.Fatalf("compatible client after rejection: %v", err)
	}
}

func TestServerBackpressureReleasesAfterWatch(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", "service.sock")
	server, stop := startTestServer(t, ServerConfig{
		SocketPath:       socketPath,
		ProtocolVersion:  1,
		ServiceName:      "test-service",
		ServiceVersion:   "1.0.0",
		MaxConcurrentRPC: 1,
	})
	defer stop()
	client := dialTestClient(t, socketPath, 1)
	defer client.Close()

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	watcher, err := client.Watch(watchCtx, Cursor{Epoch: server.Status().Epoch, Revision: server.Status().Revision})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	waitForWatchers(t, server.hub, 1)
	watchDone := make(chan error, 1)
	go func() {
		_, err := watcher.Recv()
		watchDone <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err = client.Health(ctx)
		cancel()
		if status.Code(err) == codes.ResourceExhausted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("health never observed backpressure; last error: %v", err)
		}
		time.Sleep(time.Millisecond)
	}

	cancelWatch()
	select {
	case <-watchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not stop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		_, err = client.Health(ctx)
		if err == nil {
			break
		}
		if status.Code(err) != codes.ResourceExhausted || ctx.Err() != nil {
			t.Fatalf("health after capacity release: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMaintenanceDrainAndResume(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", "service.sock")
	handler := &testMaintenanceHandler{}
	_, stop := startTestServer(t, ServerConfig{
		SocketPath:       socketPath,
		ProtocolVersion:  1,
		ServiceName:      "test-service",
		ServiceVersion:   "1.0.0",
		InitialState:     HealthReady,
		InitialAccepting: true,
		InitialDetail:    "ready",
		Maintenance:      handler,
	})
	defer stop()
	client := dialTestClient(t, socketPath, 1)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	drained, err := client.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if drained.State != HealthDraining || drained.Accepting || drained.Detail != "draining" {
		t.Fatalf("unexpected drained status: %+v", drained)
	}
	if again, err := client.Drain(ctx); err != nil || again != drained {
		t.Fatalf("idempotent drain = %+v, %v", again, err)
	}
	if got := handler.counts(); got != [2]int{1, 0} {
		t.Fatalf("handler counts after drain = %v", got)
	}

	resumed, err := client.Resume(ctx)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.State != HealthReady || !resumed.Accepting || resumed.Detail != "ready" {
		t.Fatalf("unexpected resumed status: %+v", resumed)
	}
	if again, err := client.Resume(ctx); err != nil || again != resumed {
		t.Fatalf("idempotent resume = %+v, %v", again, err)
	}
	if got := handler.counts(); got != [2]int{1, 1} {
		t.Fatalf("handler counts after resume = %v", got)
	}
}

func TestDrainTimeoutRestoresAdmissions(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", "service.sock")
	handler := &testMaintenanceHandler{drain: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	_, stop := startTestServer(t, ServerConfig{
		SocketPath:       socketPath,
		ProtocolVersion:  1,
		ServiceName:      "test-service",
		ServiceVersion:   "1.0.0",
		InitialState:     HealthReady,
		InitialAccepting: true,
		InitialDetail:    "ready",
		Maintenance:      handler,
		DrainTimeout:     20 * time.Millisecond,
	})
	defer stop()
	client := dialTestClient(t, socketPath, 1)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := client.Drain(ctx); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("drain timeout error = %v, want DeadlineExceeded", err)
	}
	health, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("health after drain timeout: %v", err)
	}
	if health.State != HealthReady || !health.Accepting || health.Detail != "ready" {
		t.Fatalf("admissions not restored: %+v", health)
	}
	if got := handler.counts(); got != [2]int{1, 1} {
		t.Fatalf("handler counts = %v", got)
	}
}

func TestFailedAutomaticResumeCanBeRetried(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", "service.sock")
	resumeAttempts := 0
	handler := &testMaintenanceHandler{
		drain: func(context.Context) error { return errors.New("blocked") },
		resume: func(context.Context) error {
			resumeAttempts++
			if resumeAttempts == 1 {
				return errors.New("temporarily unavailable")
			}
			return nil
		},
	}
	_, stop := startTestServer(t, ServerConfig{
		SocketPath:       socketPath,
		ProtocolVersion:  1,
		ServiceName:      "test-service",
		ServiceVersion:   "1.0.0",
		InitialState:     HealthReady,
		InitialAccepting: true,
		InitialDetail:    "ready",
		Maintenance:      handler,
	})
	defer stop()
	client := dialTestClient(t, socketPath, 1)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := client.Drain(ctx); status.Code(err) != codes.Internal {
		t.Fatalf("failed automatic resume error = %v, want Internal", err)
	}
	if _, err := client.Drain(ctx); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("repeated drain during recovery error = %v, want FailedPrecondition", err)
	}
	health, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("health after failed automatic resume: %v", err)
	}
	if health.State != HealthDegraded || health.Accepting {
		t.Fatalf("unsafe status after failed automatic resume: %+v", health)
	}
	resumed, err := client.Resume(ctx)
	if err != nil {
		t.Fatalf("retry resume: %v", err)
	}
	if resumed.State != HealthReady || !resumed.Accepting || resumed.Detail != "ready" {
		t.Fatalf("retry did not restore prior status: %+v", resumed)
	}
}

func TestExpiredSuccessfulDrainRestoresAdmissions(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", "service.sock")
	handler := &testMaintenanceHandler{drain: func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}}
	_, stop := startTestServer(t, ServerConfig{
		SocketPath:       socketPath,
		ProtocolVersion:  1,
		ServiceName:      "test-service",
		ServiceVersion:   "1.0.0",
		InitialState:     HealthReady,
		InitialAccepting: true,
		InitialDetail:    "ready",
		Maintenance:      handler,
		DrainTimeout:     20 * time.Millisecond,
	})
	defer stop()
	client := dialTestClient(t, socketPath, 1)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := client.Drain(ctx); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expired successful drain error = %v, want DeadlineExceeded", err)
	}
	health, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("health after expired drain: %v", err)
	}
	if health.State != HealthReady || !health.Accepting || health.Detail != "ready" {
		t.Fatalf("expired drain left admissions closed: %+v", health)
	}
	if got := handler.counts(); got != [2]int{1, 1} {
		t.Fatalf("handler counts = %v", got)
	}
}

func TestConcurrentMaintenanceIsRejected(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", "service.sock")
	started := make(chan struct{})
	release := make(chan struct{})
	handler := &testMaintenanceHandler{drain: func(context.Context) error {
		close(started)
		<-release
		return nil
	}}
	_, stop := startTestServer(t, ServerConfig{
		SocketPath:      socketPath,
		ProtocolVersion: 1,
		ServiceName:     "test-service",
		ServiceVersion:  "1.0.0",
		Maintenance:     handler,
	})
	defer stop()
	client := dialTestClient(t, socketPath, 1)
	defer client.Close()
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Drain(context.Background())
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first drain did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Resume(ctx); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("concurrent maintenance error = %v, want ResourceExhausted", err)
	}
	close(release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first drain did not finish")
	}
}

func TestPrivateSocketActiveAndStaleRecovery(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", "service.sock")
	uid := uint32(os.Getuid())
	listener, recovered, err := listenPrivateUnix(socketPath, uid)
	if err != nil || recovered {
		t.Fatalf("first listen = recovered %v, error %v", recovered, err)
	}
	if _, _, err := listenPrivateUnix(socketPath, uid); !errors.Is(err, ErrSocketActive) {
		t.Fatalf("second listen error = %v, want ErrSocketActive", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatalf("close stale socket: %v", err)
	}
	replacement, recovered, err := listenPrivateUnix(socketPath, uid)
	if err != nil || !recovered {
		t.Fatalf("stale recovery = recovered %v, error %v", recovered, err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatalf("close replacement: %v", err)
	}
}

func TestPrivateListenerClosePreservesReplacementSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", "service.sock")
	original, _, err := listenPrivateUnix(socketPath, uint32(os.Getuid()))
	if err != nil {
		t.Fatalf("listen original: %v", err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("unlink original path: %v", err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("listen replacement: %v", err)
	}
	replacement.SetUnlinkOnClose(false)
	defer func() {
		_ = replacement.Close()
		_ = os.Remove(socketPath)
	}()

	if err := original.Close(); err != nil {
		t.Fatalf("close original: %v", err)
	}
	connection, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatalf("replacement socket was removed: %v", err)
	}
	_ = connection.Close()
}

func TestConcurrentStaleSocketRecoveryHasOneWinner(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", "service.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatalf("close stale socket: %v", err)
	}

	const starters = 16
	start := make(chan struct{})
	results := make(chan struct {
		listener *privateListener
		err      error
	}, starters)
	for range starters {
		go func() {
			<-start
			listener, _, err := listenPrivateUnix(socketPath, uint32(os.Getuid()))
			results <- struct {
				listener *privateListener
				err      error
			}{listener: listener, err: err}
		}()
	}
	close(start)
	var winner *privateListener
	for range starters {
		result := <-results
		if result.err == nil {
			if winner != nil {
				_ = result.listener.Close()
				t.Fatal("multiple concurrent recoveries succeeded")
			}
			winner = result.listener
			continue
		}
		if !errors.Is(result.err, ErrSocketActive) {
			t.Fatalf("losing recovery error = %v, want ErrSocketActive", result.err)
		}
	}
	if winner == nil {
		t.Fatal("no concurrent recovery succeeded")
	}
	if err := winner.Close(); err != nil {
		t.Fatalf("close winner: %v", err)
	}
}

func TestUnixPeerCredentialsRejectUnexpectedUID(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "peer.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- connection
	}()
	client, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	var server net.Conn
	select {
	case server = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("accept: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out accepting connection")
	}
	_, _, err = (unixPeerCredentials{uid: uint32(os.Getuid()) + 1}).ServerHandshake(server)
	if err == nil || !strings.Contains(err.Error(), "peer UID") {
		t.Fatalf("wrong UID handshake error = %v", err)
	}
}

func TestStatusHubBoundsAndSlowWatcher(t *testing.T) {
	hub, err := newStatusHub(HealthStarting, false, "starting", 1)
	if err != nil {
		t.Fatalf("new status hub: %v", err)
	}
	current := hub.current()
	id, updates, initial, err := hub.subscribe(Cursor{Epoch: current.Epoch, Revision: current.Revision})
	if err != nil || initial != nil {
		t.Fatalf("subscribe current cursor = initial %+v, error %v", initial, err)
	}
	if _, _, _, err := hub.subscribe(Cursor{}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("watcher cap error = %v, want ErrBackpressure", err)
	}
	if _, err := hub.update(HealthReady, true, "ready"); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if _, err := hub.update(HealthDegraded, false, "degraded"); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if _, ok := <-updates; !ok {
		t.Fatal("buffered update was lost")
	}
	if _, ok := <-updates; ok {
		t.Fatal("slow watcher was not closed")
	}
	hub.unsubscribe(id)
	_, _, reset, err := hub.subscribe(Cursor{Epoch: current.Epoch, Revision: current.Revision})
	if err != nil {
		t.Fatalf("resubscribe: %v", err)
	}
	if reset == nil || !reset.Reset || reset.Status.Revision != 3 {
		t.Fatalf("gap did not produce current reset: %+v", reset)
	}
}

func startTestServer(t *testing.T, config ServerConfig) (*Server, func()) {
	t.Helper()
	if config.ShutdownTimeout == 0 {
		config.ShutdownTimeout = 500 * time.Millisecond
	}
	server, err := NewServer(config)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("serve: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Error("server did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return server, stop
}

func waitForWatchers(t *testing.T, hub *statusHub, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		hub.mu.Lock()
		got := len(hub.watchers)
		hub.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("watcher count = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func dialTestClient(t *testing.T, socketPath string, protocolVersion uint32) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := Dial(ctx, ClientConfig{SocketPath: socketPath, ProtocolVersion: protocolVersion, ClientName: "test-client"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return client
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %04o, want %04o", path, got, want)
	}
}

type testMaintenanceHandler struct {
	mu          sync.Mutex
	drain       func(context.Context) error
	resume      func(context.Context) error
	drainCalls  int
	resumeCalls int
}

func (h *testMaintenanceHandler) Drain(ctx context.Context) error {
	h.mu.Lock()
	h.drainCalls++
	drain := h.drain
	h.mu.Unlock()
	if drain != nil {
		return drain(ctx)
	}
	return nil
}

func (h *testMaintenanceHandler) Resume(ctx context.Context) error {
	h.mu.Lock()
	h.resumeCalls++
	resume := h.resume
	h.mu.Unlock()
	if resume != nil {
		return resume(ctx)
	}
	return nil
}

func (h *testMaintenanceHandler) counts() [2]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return [2]int{h.drainCalls, h.resumeCalls}
}
