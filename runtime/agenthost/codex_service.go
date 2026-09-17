package agenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

const (
	SupportedCodexVersion                     = "0.154.0"
	maxCodexServiceReceiptBytes               = 64 << 10
	codexServiceTimeout         time.Duration = 10 * time.Second
	codexServicePollInterval    time.Duration = 50 * time.Millisecond
)

// CodexServiceState is the last state observed for the shared service.
type CodexServiceState string

const (
	CodexServiceReady    CodexServiceState = "ready"
	CodexServiceDegraded CodexServiceState = "degraded"
)

var (
	ErrCodexServiceUnavailable  = errors.New("Codex service is unavailable")
	ErrCodexServiceIncompatible = errors.New("Codex service version is incompatible")
	// ErrCodexServiceReceipt remains exported for callers which classified the
	// former native daemon receipt. It now identifies malformed service-manager
	// observations as well.
	ErrCodexServiceReceipt              = errors.New("Codex service receipt is invalid")
	ErrCodexServiceDrift                = errors.New("Codex service identity drifted")
	ErrCodexServiceNotInstalled         = errors.New("Codex service is not installed")
	ErrCodexServiceOwnership            = errors.New("Codex service installation is not owned by herdr-plugin-kit")
	ErrCodexServiceMigrationRequired    = errors.New("Codex native daemon migration is required")
	ErrCodexServiceConfirmationRequired = errors.New("Codex service maintenance requires disconnect confirmation")
)

// CodexServiceSnapshot identifies one validated App Server instance. MainPID
// and InvocationID form the stable process identity; uptime and counters are
// deliberately not included because systemd changes them without a restart.
type CodexServiceSnapshot struct {
	State            CodexServiceState
	CodexHome        string
	SocketPath       string
	CLIVersion       string
	AppServerVersion string
	MainPID          int
	InvocationID     string
}

type codexServiceHandshake struct {
	UserAgent      string
	CodexHome      string
	PlatformFamily string
	PlatformOS     string
}

type codexServiceProcess struct {
	Executable  string
	Argv        []string
	Environment map[string]string
	Cgroup      string
}

// CodexService observes the one installed shared systemd --user App Server.
// It never invokes Codex's native daemon commands and has no ad-hoc fallback.
type CodexService struct {
	runner             command.Runner
	resolveHome        func() (string, error)
	resolvePaths       func() (codexServicePaths, error)
	lstat              func(string) (fs.FileInfo, error)
	euid               func() int
	readProcess        func(int) (codexServiceProcess, error)
	handshake          func(context.Context, string) (codexServiceHandshake, error)
	nativeDaemonActive func(context.Context, string) (bool, error)
	timeout            time.Duration
	pollInterval       time.Duration
	// afterLifecycleLockMiss is a narrow deterministic test hook for the
	// descriptor-absent race. Production instances leave it nil.
	afterLifecycleLockMiss func()
}

// NewCodexService creates a client for the installed shared service.
func NewCodexService(runner command.Runner) *CodexService {
	return &CodexService{
		runner:             runner,
		resolveHome:        effectiveCodexHome,
		resolvePaths:       defaultCodexServicePaths,
		lstat:              os.Lstat,
		euid:               os.Geteuid,
		readProcess:        readCodexServiceProcess,
		handshake:          readCodexServiceHandshake,
		nativeDaemonActive: canonicalNativeDaemonActive,
		timeout:            codexServiceTimeout,
		pollInterval:       codexServicePollInterval,
	}
}

// Ensure starts only the configured installed unit when it is stopped, then
// waits for the exact App Server instance to become ready. It never creates,
// repairs, enables, or replaces an installation.
func (s *CodexService) Ensure(ctx context.Context) (CodexServiceSnapshot, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return CodexServiceSnapshot{State: CodexServiceDegraded}, errors.Join(ErrCodexServiceUnavailable, err)
	}
	if !s.usable() {
		return CodexServiceSnapshot{State: CodexServiceDegraded}, ErrCodexServiceUnavailable
	}
	bounds, cancel := s.withTimeout(ctx)
	defer cancel()

	lockPaths, err := s.resolvePaths()
	if err != nil {
		return CodexServiceSnapshot{State: CodexServiceDegraded}, fmt.Errorf("%w: service paths: %v", ErrCodexServiceDrift, err)
	}
	lock, err := s.lifecycleLock(bounds, lockPaths, false)
	if err != nil {
		if errors.Is(err, ErrCodexServiceNotInstalled) {
			if s.afterLifecycleLockMiss != nil {
				s.afterLifecycleLockMiss()
			}
			return CodexServiceSnapshot{State: CodexServiceDegraded}, ErrCodexServiceNotInstalled
		}
		return CodexServiceSnapshot{State: CodexServiceDegraded}, errors.Join(ErrCodexServiceUnavailable, err)
	}
	defer lock.Close()

	paths, descriptor, snapshot, err := s.runtimeConfiguration()
	if err != nil {
		return snapshot, err
	}
	snapshot, show, err := s.inspectConfigured(bounds, paths, descriptor, snapshot)
	if err == nil {
		return s.finalizeReadyTransition(paths, descriptor, snapshot)
	}
	if !errors.Is(err, ErrCodexServiceUnavailable) || !show.startable() {
		return snapshot, err
	}
	if err := s.validateExecutableVersion(bounds, descriptor.Executable); err != nil {
		return snapshot, err
	}
	if _, startErr := s.runner.Run(bounds, "systemctl", []string{"--user", "start", descriptor.UnitName}); startErr != nil {
		return snapshot, errors.Join(ErrCodexServiceUnavailable, startErr)
	}
	return s.awaitReadyAndFinalizeTransition(bounds, paths, descriptor, snapshot)
}

// Inspect observes the configured service without starting, repairing, or
// changing it.
func (s *CodexService) Inspect(ctx context.Context) (CodexServiceSnapshot, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return CodexServiceSnapshot{State: CodexServiceDegraded}, errors.Join(ErrCodexServiceUnavailable, err)
	}
	if !s.usable() {
		return CodexServiceSnapshot{State: CodexServiceDegraded}, ErrCodexServiceUnavailable
	}
	bounds, cancel := s.withTimeout(ctx)
	defer cancel()
	paths, descriptor, snapshot, err := s.runtimeConfiguration()
	if err != nil {
		return snapshot, err
	}
	snapshot, _, err = s.inspectConfigured(bounds, paths, descriptor, snapshot)
	return snapshot, err
}

func (s *CodexService) usable() bool {
	return s != nil && s.runner != nil && s.resolveHome != nil && s.resolvePaths != nil
}

func (s *CodexService) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = codexServiceTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *CodexService) lifecycleLock(ctx context.Context, paths codexServicePaths, create bool) (*codexServiceLifecycleLock, error) {
	uid := os.Geteuid()
	if s.euid != nil {
		uid = s.euid()
	}
	return acquireCodexServiceLifecycleLock(ctx, paths, create, uid, s.pollInterval)
}

func (s *CodexService) runtimeConfiguration() (codexServicePaths, codexServiceDescriptor, CodexServiceSnapshot, error) {
	home, err := s.resolveHome()
	if err != nil {
		return codexServicePaths{}, codexServiceDescriptor{}, CodexServiceSnapshot{State: CodexServiceDegraded}, fmt.Errorf("%w: resolve CODEX_HOME: %v", ErrCodexServiceDrift, err)
	}
	paths, err := s.resolvePaths()
	if err != nil {
		return codexServicePaths{}, codexServiceDescriptor{}, CodexServiceSnapshot{State: CodexServiceDegraded, CodexHome: home}, fmt.Errorf("%w: service paths: %v", ErrCodexServiceDrift, err)
	}
	descriptor, err := readOwnedCodexService(paths)
	snapshot := CodexServiceSnapshot{State: CodexServiceDegraded, CodexHome: home, SocketPath: paths.SocketPath}
	if err != nil {
		return paths, codexServiceDescriptor{}, snapshot, err
	}
	if descriptor.CodexHome != home {
		return paths, descriptor, snapshot, fmt.Errorf("%w: requested CODEX_HOME %q differs from configured home", ErrCodexServiceDrift, home)
	}
	return paths, descriptor, snapshot, nil
}

func (s *CodexService) nativeDaemon(ctx context.Context, home string) (bool, error) {
	probe := s.nativeDaemonActive
	if probe == nil {
		probe = canonicalNativeDaemonActive
	}
	return probe(ctx, home)
}

func (s *CodexService) inspectConfigured(ctx context.Context, paths codexServicePaths, descriptor codexServiceDescriptor, snapshot CodexServiceSnapshot) (CodexServiceSnapshot, codexSystemdShow, error) {
	show, err := s.showUnit(ctx, descriptor.UnitName)
	if err != nil {
		return snapshot, show, err
	}
	if show.LoadState == "not-found" {
		return snapshot, show, ErrCodexServiceNotInstalled
	}
	if !loadedCodexServiceUnitMatches(paths, descriptor, show) {
		return snapshot, show, fmt.Errorf("%w: loaded unit does not match owned configuration", ErrCodexServiceDrift)
	}
	ready, err := s.inspectRunningConfiguration(ctx, paths, descriptor, snapshot, show)
	return ready, show, err
}

func loadedCodexServiceUnitMatches(paths codexServicePaths, descriptor codexServiceDescriptor, show codexSystemdShow) bool {
	return show.LoadState == "loaded" && show.FragmentPath == paths.UnitPath && systemdExecStartMatches(show.ExecStart, descriptor) && systemdEnvironmentContains(show.Environment, "CODEX_HOME", descriptor.CodexHome)
}

// inspectRunningConfiguration validates an active process and its protocol
// metadata against one exact launch configuration. It intentionally does not
// decide whether that configuration is the descriptor's desired state or the
// previous receipt-bound process during an explicit transition.
func (s *CodexService) inspectRunningConfiguration(ctx context.Context, paths codexServicePaths, descriptor codexServiceDescriptor, snapshot CodexServiceSnapshot, show codexSystemdShow) (CodexServiceSnapshot, error) {
	if show.ActiveState != "active" || show.SubState != "running" || show.MainPID <= 0 || !validInvocationID(show.InvocationID) {
		return snapshot, fmt.Errorf("%w: state=%s/%s pid=%d", ErrCodexServiceUnavailable, show.ActiveState, show.SubState, show.MainPID)
	}
	if err := s.validateProcess(descriptor, show); err != nil {
		return snapshot, err
	}
	if err := s.validateSocket(paths, descriptor); err != nil {
		return snapshot, err
	}
	handshake := s.handshake
	if handshake == nil {
		handshake = readCodexServiceHandshake
	}
	initialized, err := handshake(ctx, descriptor.SocketPath)
	if err != nil {
		return snapshot, errors.Join(ErrCodexServiceUnavailable, err)
	}
	if !codexUserAgentHasExactVersion(initialized.UserAgent, SupportedCodexVersion) || initialized.CodexHome != descriptor.CodexHome || initialized.PlatformFamily != "unix" || !strings.EqualFold(initialized.PlatformOS, "linux") {
		return snapshot, fmt.Errorf("%w: initialize metadata", ErrCodexServiceIncompatible)
	}
	snapshot.State = CodexServiceReady
	snapshot.CodexHome = descriptor.CodexHome
	snapshot.SocketPath = descriptor.SocketPath
	snapshot.CLIVersion = SupportedCodexVersion
	snapshot.AppServerVersion = SupportedCodexVersion
	snapshot.MainPID = show.MainPID
	snapshot.InvocationID = show.InvocationID
	return snapshot, nil
}

// inspectReceiptBoundPrevious validates only the process recorded when a
// configuration was staged. A different PID or invocation is never treated as
// the prior service, even if its executable and arguments happen to match.
func (s *CodexService) inspectReceiptBoundPrevious(ctx context.Context, paths codexServicePaths, descriptor codexServiceDescriptor, snapshot CodexServiceSnapshot, show codexSystemdShow) (CodexServiceSnapshot, error) {
	pending := descriptor.PendingTransition
	if pending == nil {
		return snapshot, fmt.Errorf("%w: no pending transition", ErrCodexServiceDrift)
	}
	if show.MainPID != pending.MainPID || show.InvocationID != pending.InvocationID {
		return snapshot, fmt.Errorf("%w: pending transition process identity", ErrCodexServiceDrift)
	}
	return s.inspectRunningConfiguration(ctx, paths, pending.previousDescriptor(descriptor), snapshot, show)
}

func codexUserAgentHasExactVersion(userAgent, version string) bool {
	marker := "/" + version
	for {
		_, after, found := strings.Cut(userAgent, marker)
		if !found {
			return false
		}
		if after == "" || after[0] == ' ' || after[0] == '\t' || after[0] == '(' {
			return true
		}
		userAgent = after
	}
}

func (s *CodexService) awaitReady(ctx context.Context, paths codexServicePaths, descriptor codexServiceDescriptor, snapshot CodexServiceSnapshot) (CodexServiceSnapshot, error) {
	for {
		ready, _, err := s.inspectConfigured(ctx, paths, descriptor, snapshot)
		if err == nil {
			return ready, nil
		}
		if !errors.Is(err, ErrCodexServiceUnavailable) {
			return ready, err
		}
		interval := s.pollInterval
		if interval <= 0 {
			interval = codexServicePollInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ready, errors.Join(ErrCodexServiceUnavailable, ctx.Err())
		case <-timer.C:
		}
	}
}

func (s *CodexService) awaitReadyAndFinalizeTransition(ctx context.Context, paths codexServicePaths, descriptor codexServiceDescriptor, snapshot CodexServiceSnapshot) (CodexServiceSnapshot, error) {
	ready, err := s.awaitReady(ctx, paths, descriptor, snapshot)
	if err != nil {
		return ready, err
	}
	return s.finalizeReadyTransition(paths, descriptor, ready)
}

func (s *CodexService) showUnit(ctx context.Context, unit string) (codexSystemdShow, error) {
	result, runErr := s.runner.Run(ctx, "systemctl", []string{
		"--user", "show", unit,
		"--property=LoadState,UnitFileState,ActiveState,SubState,MainPID,InvocationID,FragmentPath,ExecStart,Environment",
		"--no-page",
	})
	show, parseErr := parseCodexSystemdShow(result.Stdout, result.StdoutTruncated || result.StderrTruncated)
	if parseErr != nil {
		if runErr != nil {
			return show, errors.Join(ErrCodexServiceUnavailable, runErr, parseErr)
		}
		return show, parseErr
	}
	if runErr != nil && show.LoadState != "not-found" {
		return show, errors.Join(ErrCodexServiceUnavailable, runErr)
	}
	return show, nil
}

func (s *CodexService) validateProcess(descriptor codexServiceDescriptor, show codexSystemdShow) error {
	reader := s.readProcess
	if reader == nil {
		reader = readCodexServiceProcess
	}
	process, err := reader(show.MainPID)
	if err != nil {
		return errors.Join(ErrCodexServiceUnavailable, err)
	}
	if process.Executable != descriptor.Executable {
		return fmt.Errorf("%w: service executable", ErrCodexServiceDrift)
	}
	wantArgv := []string{descriptor.Executable, "app-server", "--listen", "unix://" + descriptor.SocketPath}
	if !sameStrings(process.Argv, wantArgv) {
		return fmt.Errorf("%w: service argv", ErrCodexServiceDrift)
	}
	if process.Environment["CODEX_HOME"] != descriptor.CodexHome {
		return fmt.Errorf("%w: service CODEX_HOME", ErrCodexServiceDrift)
	}
	if !cgroupContainsUnit(process.Cgroup, descriptor.UnitName) {
		return fmt.Errorf("%w: service cgroup", ErrCodexServiceDrift)
	}
	return nil
}

func cgroupContainsUnit(cgroup, unit string) bool {
	for _, line := range strings.Split(cgroup, "\n") {
		path := line[strings.LastIndexByte(line, ':')+1:]
		for _, component := range strings.Split(path, "/") {
			if component == unit {
				return true
			}
		}
	}
	return false
}

func (s *CodexService) validateSocket(paths codexServicePaths, descriptor codexServiceDescriptor) error {
	lstat := s.lstat
	if lstat == nil {
		lstat = os.Lstat
	}
	euid := os.Geteuid
	if s.euid != nil {
		euid = s.euid
	}
	directory, err := lstat(paths.RuntimeDir)
	if errors.Is(err, os.ErrNotExist) {
		// inspectConfigured has already validated the exact active process.
		// systemd may report it running just before RuntimeDirectory appears.
		return errors.Join(ErrCodexServiceUnavailable, err)
	}
	if err != nil || directory.Mode()&os.ModeSymlink != 0 || !directory.IsDir() || directory.Mode().Perm() != 0o700 || !ownedBy(directory, euid()) {
		return fmt.Errorf("%w: runtime directory", ErrCodexServiceDrift)
	}
	socket, err := lstat(descriptor.SocketPath)
	if errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrCodexServiceUnavailable, err)
	}
	if err != nil || socket.Mode()&os.ModeSymlink != 0 || socket.Mode()&os.ModeSocket == 0 || socket.Mode().Perm() != 0o600 || !ownedBy(socket, euid()) {
		return fmt.Errorf("%w: socket", ErrCodexServiceDrift)
	}
	return nil
}

func ownedBy(info fs.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}

func readCodexServiceProcess(pid int) (codexServiceProcess, error) {
	return readCodexServiceProcessAt("/proc", pid)
}

// readCodexServiceProcessAt keeps the production reader pinned to /proc while
// allowing filesystem fixtures to exercise malformed and unavailable process
// observations without reading a test runner's environment.
func readCodexServiceProcessAt(procRoot string, pid int) (codexServiceProcess, error) {
	if pid <= 0 {
		return codexServiceProcess{}, errors.New("invalid process ID")
	}
	base := filepath.Join(procRoot, strconv.Itoa(pid))
	executable, err := os.Readlink(filepath.Join(base, "exe"))
	if err != nil {
		return codexServiceProcess{}, err
	}
	cmdline, err := readSmallProcFile(filepath.Join(base, "cmdline"))
	if err != nil {
		return codexServiceProcess{}, err
	}
	environ, err := readSmallProcFile(filepath.Join(base, "environ"))
	if err != nil {
		return codexServiceProcess{}, err
	}
	cgroup, err := readSmallProcFile(filepath.Join(base, "cgroup"))
	if err != nil {
		return codexServiceProcess{}, err
	}
	argv := splitNUL(cmdline)
	if len(argv) == 0 {
		return codexServiceProcess{}, errors.New("empty process argv")
	}
	values := make(map[string]string)
	for _, item := range splitNUL(environ) {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return codexServiceProcess{}, errors.New("duplicate process environment entry")
		}
		values[key] = value
	}
	return codexServiceProcess{Executable: executable, Argv: argv, Environment: values, Cgroup: string(cgroup)}, nil
}

func readSmallProcFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxCodexServiceReceiptBytes {
		return nil, errors.New("process observation is too large")
	}
	return data, nil
}

func splitNUL(data []byte) []string {
	parts := bytes.Split(data, []byte{0})
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) != 0 {
			values = append(values, string(part))
		}
	}
	return values
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type codexSystemdShow struct {
	LoadState     string
	UnitFileState string
	ActiveState   string
	SubState      string
	MainPID       int
	InvocationID  string
	FragmentPath  string
	ExecStart     string
	Environment   string
}

func (s codexSystemdShow) startable() bool {
	return s.LoadState == "loaded" && (s.ActiveState == "inactive" || s.ActiveState == "failed")
}

func parseCodexSystemdShow(raw []byte, truncated bool) (codexSystemdShow, error) {
	if truncated || len(raw) == 0 || len(raw) > maxCodexServiceReceiptBytes {
		return codexSystemdShow{}, ErrCodexServiceReceipt
	}
	required := map[string]bool{
		"LoadState": false, "UnitFileState": false, "ActiveState": false, "SubState": false,
		"MainPID": false, "InvocationID": false, "FragmentPath": false, "ExecStart": false, "Environment": false,
	}
	values := make(map[string]string, len(required))
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return codexSystemdShow{}, fmt.Errorf("%w: malformed systemctl show line", ErrCodexServiceReceipt)
		}
		if _, known := required[key]; !known {
			continue
		}
		if required[key] {
			return codexSystemdShow{}, fmt.Errorf("%w: duplicate %s", ErrCodexServiceReceipt, key)
		}
		required[key] = true
		values[key] = value
	}
	for key, present := range required {
		// systemctl omits ExecStart altogether for a not-found unit rather
		// than emitting ExecStart=. All other observations remain required.
		if !present && !(key == "ExecStart" && values["LoadState"] == "not-found") {
			return codexSystemdShow{}, fmt.Errorf("%w: missing %s", ErrCodexServiceReceipt, key)
		}
	}
	pid, err := strconv.Atoi(values["MainPID"])
	if err != nil || pid < 0 {
		return codexSystemdShow{}, fmt.Errorf("%w: MainPID", ErrCodexServiceReceipt)
	}
	return codexSystemdShow{
		LoadState: values["LoadState"], UnitFileState: values["UnitFileState"], ActiveState: values["ActiveState"], SubState: values["SubState"],
		MainPID: pid, InvocationID: values["InvocationID"], FragmentPath: values["FragmentPath"], ExecStart: values["ExecStart"], Environment: values["Environment"],
	}, nil
}

func validInvocationID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func systemdEnvironmentContains(value, key, want string) bool {
	entries, ok := parseSystemdEnvironment(value)
	if !ok {
		return false
	}
	wantEntry := key + "=" + want
	found := false
	for _, entry := range entries {
		entryKey, _, hasAssignment := strings.Cut(entry, "=")
		if !hasAssignment || entryKey != key {
			continue
		}
		if found || entry != wantEntry {
			return false
		}
		found = true
	}
	return found
}

// parseSystemdEnvironment decodes the whitespace-separated, double-quoted
// Environment= value emitted by systemctl show. It intentionally fails closed
// on an unfamiliar escape rather than treating a substring as ownership proof.
func parseSystemdEnvironment(value string) ([]string, bool) {
	entries := make([]string, 0, 1)
	for index := 0; index < len(value); {
		for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
			index++
		}
		if index == len(value) {
			break
		}
		var entry strings.Builder
		quoted := false
		started := false
		for index < len(value) {
			char := value[index]
			if !quoted && (char == ' ' || char == '\t') {
				break
			}
			started = true
			index++
			switch char {
			case '"':
				quoted = !quoted
			case '\\':
				if index == len(value) {
					return nil, false
				}
				escaped := value[index]
				index++
				switch escaped {
				case '\\', '"':
					entry.WriteByte(escaped)
				case 'n':
					entry.WriteByte('\n')
				case 'r':
					entry.WriteByte('\r')
				case 't':
					entry.WriteByte('\t')
				case 'x':
					if index+2 > len(value) {
						return nil, false
					}
					decoded, err := strconv.ParseUint(value[index:index+2], 16, 8)
					if err != nil {
						return nil, false
					}
					entry.WriteByte(byte(decoded))
					index += 2
				default:
					return nil, false
				}
			default:
				if char == '\n' || char == '\r' {
					return nil, false
				}
				entry.WriteByte(char)
			}
		}
		if quoted || !started {
			return nil, false
		}
		entries = append(entries, entry.String())
	}
	return entries, true
}

func effectiveCodexHome() (string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".codex")
	}
	return canonicalExistingDirectory(home, false)
}

func canonicalExistingDirectory(path string, requireAbsolute bool) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || (requireAbsolute && !filepath.IsAbs(path)) {
		return "", errors.New("path must be absolute, non-empty, and NUL-free")
	}
	if !filepath.IsAbs(path) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = absolute
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("path is not an existing directory")
	}
	return filepath.Clean(canonical), nil
}

func canonicalNativeDaemonActive(ctx context.Context, home string) (bool, error) {
	socket := filepath.Join(home, "app-server-control", "app-server-control.sock")
	info, err := os.Lstat(socket)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	if info.Mode()&os.ModeSocket == 0 {
		return false, nil
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return false, nil
		}
		return false, err
	}
	return true, connection.Close()
}

func readCodexServiceHandshake(ctx context.Context, socket string) (codexServiceHandshake, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	if err != nil {
		return codexServiceHandshake{}, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(codexMessageBytes)
	reader := codexReader{conn: conn}
	request := []byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"herdr-plugin-kit","version":"0.3.0"},"capabilities":{"experimentalApi":true}}}`)
	if err := conn.Write(ctx, websocket.MessageText, request); err != nil {
		return codexServiceHandshake{}, err
	}
	raw, err := reader.response(ctx, "1")
	if err != nil {
		return codexServiceHandshake{}, err
	}
	var initialized codexServiceHandshake
	if json.Unmarshal(raw, &initialized) != nil || initialized.UserAgent == "" || initialized.CodexHome == "" || initialized.PlatformFamily == "" || initialized.PlatformOS == "" {
		return codexServiceHandshake{}, errCodexProtocol
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"method":"initialized"}`)); err != nil {
		return codexServiceHandshake{}, err
	}
	return initialized, nil
}

// inspectCodexServiceSocket is retained for internal compatibility with the
// existing protocol tests. Runtime validation uses the richer metadata above.
func inspectCodexServiceSocket(ctx context.Context, socket string) error {
	_, err := readCodexServiceHandshake(ctx, socket)
	return err
}

func cleanAbsolute(path string) bool {
	return path != "" && len(path) <= maxPathBytes && !strings.ContainsRune(path, 0) && filepath.IsAbs(path) && filepath.Clean(path) == path
}
