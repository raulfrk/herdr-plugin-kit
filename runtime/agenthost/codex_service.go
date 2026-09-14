package agenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

const (
	SupportedCodexVersion                     = "0.154.0"
	maxCodexServiceReceiptBytes               = 64 << 10
	codexServiceTimeout         time.Duration = 10_000_000_000 // 10 seconds
)

// CodexServiceState is the last state observed for the native shared service.
type CodexServiceState string

const (
	CodexServiceReady    CodexServiceState = "ready"
	CodexServiceDegraded CodexServiceState = "degraded"
)

var (
	ErrCodexServiceUnavailable  = errors.New("Codex service is unavailable")
	ErrCodexServiceIncompatible = errors.New("Codex service version is incompatible")
	ErrCodexServiceReceipt      = errors.New("Codex service receipt is invalid")
	ErrCodexServiceDrift        = errors.New("Codex service identity drifted")
)

// CodexServiceSnapshot identifies one validated native daemon without owning it.
type CodexServiceSnapshot struct {
	State            CodexServiceState
	CodexHome        string
	SocketPath       string
	CLIVersion       string
	AppServerVersion string
}

// CodexService starts or inspects Codex's native per-home App Server daemon.
// It deliberately has no stop, restart, bootstrap, update, or watch operation.
type CodexService struct {
	runner      command.Runner
	executable  string
	resolveHome func() (string, error)
	evalPath    func(string) (string, error)
	lstat       func(string) (fs.FileInfo, error)
	euid        func() int
	handshake   func(context.Context, string) error
	timeout     time.Duration
}

// NewCodexService creates a plugin-neutral client for the native Codex service.
func NewCodexService(runner command.Runner) *CodexService {
	return &CodexService{
		runner:      runner,
		executable:  "codex",
		resolveHome: effectiveCodexHome,
		evalPath:    filepath.EvalSymlinks,
		lstat:       os.Lstat,
		euid:        os.Geteuid,
		handshake:   inspectCodexServiceSocket,
		timeout:     codexServiceTimeout,
	}
}

// Ensure asks Codex to start its native daemon if absent and validates the
// resulting shared service. Concurrent callers rely on Codex's native locking.
func (s *CodexService) Ensure(ctx context.Context) (CodexServiceSnapshot, error) {
	return s.observe(ctx, "start", "started", "alreadyRunning")
}

// Inspect observes the native daemon without starting or repairing it.
func (s *CodexService) Inspect(ctx context.Context) (CodexServiceSnapshot, error) {
	return s.observe(ctx, "version", "running")
}

func (s *CodexService) observe(ctx context.Context, operation string, statuses ...string) (CodexServiceSnapshot, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return CodexServiceSnapshot{State: CodexServiceDegraded}, errors.Join(ErrCodexServiceUnavailable, err)
	}
	if s == nil || s.runner == nil || s.resolveHome == nil {
		return CodexServiceSnapshot{State: CodexServiceDegraded}, ErrCodexServiceUnavailable
	}
	timeout := s.timeout
	if timeout <= 0 {
		timeout = codexServiceTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	home, err := s.resolveHome()
	if err != nil {
		return CodexServiceSnapshot{State: CodexServiceDegraded}, fmt.Errorf("%w: resolve CODEX_HOME: %v", ErrCodexServiceDrift, err)
	}
	snapshot := CodexServiceSnapshot{State: CodexServiceDegraded, CodexHome: home}
	executable := s.executable
	if executable == "" {
		executable = "codex"
	}
	result, err := s.runner.Run(bounded, executable, []string{"app-server", "daemon", operation})
	if err != nil {
		return snapshot, errors.Join(ErrCodexServiceUnavailable, err)
	}
	receipt, err := decodeCodexServiceReceipt(result)
	if err != nil {
		return snapshot, err
	}
	if !oneOf(receipt.Status, statuses) {
		return snapshot, fmt.Errorf("%w: status %q", ErrCodexServiceReceipt, receipt.Status)
	}
	if receipt.Status == "started" {
		var pid int
		if len(receipt.PID) == 0 || string(receipt.PID) == "null" || json.Unmarshal(receipt.PID, &pid) != nil || pid <= 0 {
			return snapshot, fmt.Errorf("%w: started receipt PID", ErrCodexServiceReceipt)
		}
	} else if len(receipt.PID) != 0 {
		return snapshot, fmt.Errorf("%w: unexpected PID", ErrCodexServiceReceipt)
	}
	snapshot.CLIVersion = receipt.CLIVersion
	snapshot.AppServerVersion = receipt.AppServerVersion
	if receipt.CLIVersion != SupportedCodexVersion || receipt.ManagedCodexVersion != SupportedCodexVersion || receipt.AppServerVersion != SupportedCodexVersion {
		return snapshot, fmt.Errorf("%w: CLI=%q managed=%q app-server=%q", ErrCodexServiceIncompatible, receipt.CLIVersion, receipt.ManagedCodexVersion, receipt.AppServerVersion)
	}
	socket, err := s.validateIdentity(bounded, home, receipt)
	if err != nil {
		return snapshot, err
	}
	snapshot.SocketPath = socket
	snapshot.State = CodexServiceReady
	return snapshot, nil
}

type codexServiceReceipt struct {
	Status              string          `json:"status"`
	Backend             string          `json:"backend"`
	PID                 json.RawMessage `json:"pid,omitempty"`
	ManagedCodexPath    string          `json:"managedCodexPath"`
	ManagedCodexVersion string          `json:"managedCodexVersion"`
	SocketPath          string          `json:"socketPath"`
	CLIVersion          string          `json:"cliVersion"`
	AppServerVersion    string          `json:"appServerVersion"`
}

func decodeCodexServiceReceipt(result command.Result) (codexServiceReceipt, error) {
	if result.StdoutTruncated || result.StderrTruncated || len(result.Stdout) == 0 || len(result.Stdout) > maxCodexServiceReceiptBytes {
		return codexServiceReceipt{}, ErrCodexServiceReceipt
	}
	if err := validateCodexServiceReceiptShape(result.Stdout); err != nil {
		return codexServiceReceipt{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(result.Stdout))
	decoder.DisallowUnknownFields()
	var receipt codexServiceReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return codexServiceReceipt{}, fmt.Errorf("%w: %v", ErrCodexServiceReceipt, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return codexServiceReceipt{}, fmt.Errorf("%w: expected one JSON object", ErrCodexServiceReceipt)
	}
	for name, value := range map[string]string{
		"status": receipt.Status, "backend": receipt.Backend, "managedCodexPath": receipt.ManagedCodexPath,
		"managedCodexVersion": receipt.ManagedCodexVersion, "socketPath": receipt.SocketPath,
		"cliVersion": receipt.CLIVersion, "appServerVersion": receipt.AppServerVersion,
	} {
		if value == "" || strings.ContainsRune(value, 0) {
			return codexServiceReceipt{}, fmt.Errorf("%w: %s", ErrCodexServiceReceipt, name)
		}
	}
	if receipt.Backend != "pid" {
		return codexServiceReceipt{}, fmt.Errorf("%w: backend %q", ErrCodexServiceDrift, receipt.Backend)
	}
	return receipt, nil
}

func validateCodexServiceReceiptShape(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	delim, ok := token.(json.Delim)
	if err != nil || !ok || delim != '{' {
		return fmt.Errorf("%w: expected JSON object", ErrCodexServiceReceipt)
	}
	required := map[string]bool{
		"status": false, "backend": false, "managedCodexPath": false,
		"managedCodexVersion": false, "socketPath": false,
		"cliVersion": false, "appServerVersion": false,
	}
	seen := make(map[string]struct{}, len(required)+1)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return fmt.Errorf("%w: expected field name", ErrCodexServiceReceipt)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("%w: duplicate field %q", ErrCodexServiceReceipt, name)
		}
		if _, known := required[name]; !known && name != "pid" {
			return fmt.Errorf("%w: unknown field %q", ErrCodexServiceReceipt, name)
		}
		seen[name] = struct{}{}
		if _, known := required[name]; known {
			required[name] = true
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil || len(value) == 0 {
			return fmt.Errorf("%w: field %q", ErrCodexServiceReceipt, name)
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return fmt.Errorf("%w: unterminated object", ErrCodexServiceReceipt)
	}
	if _, err = decoder.Token(); err != io.EOF {
		return fmt.Errorf("%w: expected one JSON object", ErrCodexServiceReceipt)
	}
	for name, present := range required {
		if !present {
			return fmt.Errorf("%w: missing field %q", ErrCodexServiceReceipt, name)
		}
	}
	return nil
}

func (s *CodexService) validateIdentity(ctx context.Context, home string, receipt codexServiceReceipt) (string, error) {
	if !cleanAbsolute(receipt.ManagedCodexPath) || !cleanAbsolute(receipt.SocketPath) {
		return "", ErrCodexServiceDrift
	}
	eval := s.evalPath
	if eval == nil {
		eval = filepath.EvalSymlinks
	}
	managed, err := eval(receipt.ManagedCodexPath)
	if err != nil {
		return "", fmt.Errorf("%w: managed executable: %v", ErrCodexServiceDrift, err)
	}
	expectedManaged, err := eval(filepath.Join(home, "packages", "standalone", "current", "bin", "codex"))
	if err != nil || managed != expectedManaged {
		return "", fmt.Errorf("%w: managed executable", ErrCodexServiceDrift)
	}
	socket, err := eval(receipt.SocketPath)
	if err != nil || socket != filepath.Join(home, "app-server-control", "app-server-control.sock") {
		return "", fmt.Errorf("%w: socket path", ErrCodexServiceDrift)
	}
	lstat := s.lstat
	if lstat == nil {
		lstat = os.Lstat
	}
	info, err := lstat(socket)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%w: socket mode", ErrCodexServiceDrift)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	euid := os.Geteuid
	if s.euid != nil {
		euid = s.euid
	}
	if !ok || int(stat.Uid) != euid() {
		return "", fmt.Errorf("%w: socket owner", ErrCodexServiceDrift)
	}
	handshake := s.handshake
	if handshake == nil {
		handshake = inspectCodexServiceSocket
	}
	if err := handshake(ctx, socket); err != nil {
		return "", errors.Join(ErrCodexServiceUnavailable, err)
	}
	return socket, nil
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
	if strings.ContainsRune(home, 0) {
		return "", errors.New("CODEX_HOME contains NUL")
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("CODEX_HOME is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func inspectCodexServiceSocket(ctx context.Context, socket string) error {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(codexMessageBytes)
	reader := codexReader{conn: conn}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"herdr-plugin-kit","version":"0.3.0"},"capabilities":{"experimentalApi":true}}}`)); err != nil {
		return err
	}
	raw, err := reader.response(ctx, "1")
	if err != nil {
		return err
	}
	var initialized struct {
		UserAgent string `json:"userAgent"`
	}
	if json.Unmarshal(raw, &initialized) != nil || initialized.UserAgent == "" {
		return errCodexProtocol
	}
	return conn.Write(ctx, websocket.MessageText, []byte(`{"method":"initialized"}`))
}

func cleanAbsolute(path string) bool {
	return path != "" && len(path) <= maxPathBytes && !strings.ContainsRune(path, 0) && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func oneOf(value string, choices []string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
