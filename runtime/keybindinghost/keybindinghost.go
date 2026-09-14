// Package keybindinghost manages revision-checked Herdr keybindings owned by
// Plugin Kit consumers.
package keybindinghost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
	"github.com/raulfrk/herdr-plugin-kit/documentstore"
	"github.com/raulfrk/herdr-plugin-kit/manifest"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrcmd"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrid"
)

const (
	MaxBindings                     = manifest.MaxActions
	MaxKeyBytes                     = 128
	MaxDescription                  = 1024
	MaxConfigBytes                  = 1 << 20
	rollbackTimeout   time.Duration = 5_000_000_000
	environmentBinary               = "/usr/bin/env"

	markerPrefix = "# herdr-plugin-kit keybindings v1 "
	beginNone    = "# herdr-plugin-kit keybindings v1 begin separator=none"
	beginLF      = "# herdr-plugin-kit keybindings v1 begin separator=lf"
	ownerPrefix  = "# herdr-plugin-kit keybindings v1 owner="
	endMarker    = "# herdr-plugin-kit keybindings v1 end"
)

var (
	ErrInvalid   = errors.New("invalid keybinding transaction")
	ErrOwnership = errors.New("invalid managed keybinding ownership")
	ErrCollision = errors.New("Herdr keybinding collision")
	ErrDrift     = errors.New("Herdr config revision drift")
	ErrCheck     = errors.New("Herdr config check failed")
	ErrWrite     = errors.New("Herdr config write failed")
	ErrReload    = errors.New("Herdr config reload failed")
	ErrRollback  = errors.New("Herdr keybinding rollback failed")
)

// FailureKind identifies the phase that prevented a transaction from
// completing.
type FailureKind string

const (
	FailureInvalid   FailureKind = "invalid"
	FailureOwnership FailureKind = "ownership"
	FailureCollision FailureKind = "collision"
	FailureDrift     FailureKind = "drift"
	FailureCheck     FailureKind = "check"
	FailureWrite     FailureKind = "write"
	FailureReload    FailureKind = "reload"
	FailureRollback  FailureKind = "rollback"
	FailureCanceled  FailureKind = "canceled"
)

// Error is a typed transaction failure. Diagnostics contains bounded Herdr
// stdout/stderr when a CLI phase fails.
type Error struct {
	Kind        FailureKind
	Diagnostics string
	Cause       error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := string(e.Kind)
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	if e.Diagnostics != "" {
		message += ": " + e.Diagnostics
	}
	return message
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool {
	want := sentinelKind(target)
	return want != "" && want == e.Kind
}

func sentinelKind(target error) FailureKind {
	switch target {
	case ErrInvalid:
		return FailureInvalid
	case ErrOwnership:
		return FailureOwnership
	case ErrCollision:
		return FailureCollision
	case ErrDrift:
		return FailureDrift
	case ErrCheck:
		return FailureCheck
	case ErrWrite:
		return FailureWrite
	case ErrReload:
		return FailureReload
	case ErrRollback:
		return FailureRollback
	default:
		return ""
	}
}

// Binding maps one Herdr key to a predeclared fixed plugin action.
type Binding struct {
	Key         string
	Action      string
	Description string
}

// Snapshot is the caller-owned portion of one coherent config read.
type Snapshot struct {
	Revision documentstore.Revision
	Bindings []Binding
}

// Request replaces all bindings owned by Owner when Expected still identifies
// the exact config bytes observed by the caller.
type Request struct {
	Owner    string
	Expected documentstore.Revision
	Bindings []Binding
}

// Result reports both the durable revision and any recovery performed after a
// failed reload.
type Result struct {
	Revision          documentstore.Revision
	Changed           bool
	RolledBack        bool
	EffectiveRestored bool
}

// Host addresses one exact Herdr config and default or named session.
type Host struct {
	Runner     command.Runner
	Herdr      string
	ConfigPath string
	// Session is empty for the default session. The literal name "default" is
	// rejected because it is not a named-session alias.
	Session     string
	openStore   func(string) (documentStore, string, error)
	acquireLock func(context.Context, string) (io.Closer, error)
}

type documentStore interface {
	Close() error
	ReadBounded(context.Context, string, int) (documentstore.Document, error)
	WriteBounded(context.Context, string, []byte, documentstore.Revision, int) (documentstore.Revision, error)
}

// EffectiveConfigPath resolves the same supported override/default path used
// by Herdr 0.8.2.
func EffectiveConfigPath() (string, error) {
	if configured := os.Getenv("HERDR_CONFIG_PATH"); configured != "" {
		return validateConfigPath(configured)
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return validateConfigPath(filepath.Join(root, "herdr", "config.toml"))
}

// Read returns the exact revision and bindings currently owned by owner.
func (h Host) Read(ctx context.Context, owner string) (Snapshot, error) {
	if err := validateOwner(owner); err != nil {
		return Snapshot{}, failure(FailureInvalid, err, "")
	}
	path, err := h.validate()
	if err != nil {
		return Snapshot{}, err
	}
	store, name, err := h.store(path)
	if err != nil {
		return Snapshot{}, err
	}
	defer store.Close()
	document, err := store.ReadBounded(ctx, name, MaxConfigBytes)
	if err != nil {
		if errors.Is(err, documentstore.ErrTooLarge) {
			return Snapshot{}, failure(FailureInvalid, fmt.Errorf("Herdr config exceeds %d bytes", MaxConfigBytes), "")
		}
		return Snapshot{}, err
	}
	managed, err := parseManaged(document.Bytes)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Revision: document.Revision, Bindings: slices.Clone(managed.bindings(owner))}, nil
}

// Apply atomically replaces the caller-owned bindings and reloads only the
// addressed Herdr session. A failed reload triggers an exact-byte rollback and
// a bounded attempt to restore the prior effective keymap.
func (h Host) Apply(ctx context.Context, request Request) (Result, error) {
	if err := validateOwner(request.Owner); err != nil {
		return Result{}, failure(FailureInvalid, err, "")
	}
	if err := validateBindings(request.Owner, request.Bindings); err != nil {
		return Result{}, err
	}
	path, err := h.validate()
	if err != nil {
		return Result{}, err
	}
	transaction, err := h.transactionLock(ctx, path)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Result{}, failure(FailureCanceled, err, "")
		}
		return Result{}, failure(FailureWrite, fmt.Errorf("lock keybinding transaction: %w", err), "")
	}
	defer transaction.Close()
	store, name, err := h.store(path)
	if err != nil {
		return Result{}, err
	}
	defer store.Close()

	previous, err := store.ReadBounded(ctx, name, MaxConfigBytes)
	if err != nil {
		if errors.Is(err, documentstore.ErrTooLarge) {
			return Result{}, failure(FailureInvalid, fmt.Errorf("Herdr config exceeds %d bytes", MaxConfigBytes), "")
		}
		return Result{}, err
	}
	if previous.Revision != request.Expected {
		return Result{Revision: previous.Revision}, failure(FailureDrift, documentstore.ErrConflict, "")
	}
	nextBytes, changed, err := replaceOwned(previous.Bytes, request.Owner, request.Bindings)
	if err != nil {
		return Result{Revision: previous.Revision}, err
	}
	if !changed {
		return Result{Revision: previous.Revision}, nil
	}
	if len(nextBytes) > MaxConfigBytes {
		return Result{Revision: previous.Revision}, failure(FailureInvalid, fmt.Errorf("Herdr config exceeds %d bytes", MaxConfigBytes), "")
	}
	if err := h.checkCandidate(ctx, path, nextBytes); err != nil {
		return Result{Revision: previous.Revision}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{Revision: previous.Revision}, failure(FailureCanceled, err, "")
	}
	nextRevision, err := store.WriteBounded(ctx, name, nextBytes, previous.Revision, MaxConfigBytes)
	if errors.Is(err, documentstore.ErrConflict) {
		return Result{}, failure(FailureDrift, err, "")
	}
	if err != nil {
		if nextRevision == (documentstore.Revision{}) {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Result{}, failure(FailureCanceled, err, "")
			}
			return Result{}, failure(FailureWrite, err, "")
		}
		result := Result{Revision: nextRevision, Changed: true}
		return h.rollback(ctx, store, name, path, previous, nextRevision, result, failure(FailureWrite, err, ""))
	}
	result := Result{Revision: nextRevision, Changed: true}
	if reloadErr := h.reload(ctx, path); reloadErr == nil {
		return result, nil
	} else {
		return h.rollback(ctx, store, name, path, previous, nextRevision, result, reloadErr)
	}
}

func (h Host) rollback(parent context.Context, store documentStore, name, path string, previous documentstore.Document, written documentstore.Revision, result Result, reloadErr error) (Result, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), rollbackTimeout)
	defer cancel()
	restored, writeErr := store.WriteBounded(ctx, name, previous.Bytes, written, MaxConfigBytes)
	if writeErr != nil {
		if restored == (documentstore.Revision{}) {
			return result, failure(FailureRollback, errors.Join(reloadErr, writeErr), "")
		}
		result.Revision = restored
		result.RolledBack = true
		restoreErr := h.reload(ctx, path)
		if restoreErr == nil {
			result.EffectiveRestored = true
		}
		return result, failure(FailureRollback, errors.Join(reloadErr, writeErr, restoreErr), "")
	}
	result.Revision = restored
	result.RolledBack = true
	if restoreErr := h.reload(ctx, path); restoreErr != nil {
		return result, failure(FailureRollback, errors.Join(reloadErr, restoreErr), "")
	}
	result.EffectiveRestored = true
	kind := FailureReload
	var operation *Error
	if errors.As(reloadErr, &operation) {
		kind = operation.Kind
	}
	return result, failure(kind, reloadErr, "")
}

func (h Host) checkCandidate(ctx context.Context, configPath string, data []byte) (resultErr error) {
	directory := filepath.Dir(configPath)
	file, err := os.CreateTemp(directory, ".herdr-plugin-kit-check-*")
	if err != nil {
		return failure(FailureCheck, fmt.Errorf("create config candidate: %w", err), "")
	}
	path := file.Name()
	defer func() {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, failure(FailureCheck, fmt.Errorf("remove config candidate: %w", removeErr), ""))
		}
	}()
	closeWith := func(cause error) error {
		return errors.Join(cause, file.Close())
	}
	if err := file.Chmod(0o600); err != nil {
		return failure(FailureCheck, closeWith(err), "")
	}
	if written, err := file.Write(data); err != nil {
		return failure(FailureCheck, closeWith(err), "")
	} else if written != len(data) {
		return failure(FailureCheck, closeWith(io.ErrShortWrite), "")
	}
	if err := file.Sync(); err != nil {
		return failure(FailureCheck, closeWith(err), "")
	}
	if err := file.Close(); err != nil {
		return failure(FailureCheck, err, "")
	}
	result, err := h.run(ctx, path, []string{"config", "check"})
	if err != nil {
		return classifyCommandFailure(FailureCheck, err, result)
	}
	if !bytes.Equal(result.Stdout, []byte("config: ok\n")) || len(result.Stderr) != 0 {
		return failure(FailureCheck, errors.New("unexpected config-check response"), diagnostics(result))
	}
	return nil
}

func (h Host) reload(ctx context.Context, configPath string) error {
	args := []string{"server", "reload-config"}
	if h.Session != "" {
		args = append([]string{"--session", h.Session}, args...)
	}
	result, err := h.run(ctx, configPath, args)
	if err != nil {
		return classifyCommandFailure(FailureReload, err, result)
	}
	if len(result.Stderr) != 0 || len(result.Stdout) > herdrcmd.MaxJSONBytes {
		return failure(FailureReload, errors.New("invalid config-reload response"), diagnostics(result))
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type        string            `json:"type"`
			Status      string            `json:"status"`
			Diagnostics []json.RawMessage `json:"diagnostics"`
		} `json:"result"`
	}
	decoder := json.NewDecoder(bytes.NewReader(result.Stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return failure(FailureReload, fmt.Errorf("decode config-reload response: %w", err), diagnostics(result))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return failure(FailureReload, errors.New("config-reload response must contain one JSON value"), diagnostics(result))
	}
	if response.ID != "cli:server:reload-config" || response.Result.Type != "config_reload" || response.Result.Status != "applied" || len(response.Result.Diagnostics) != 0 {
		return failure(FailureReload, errors.New("unexpected config-reload response"), diagnostics(result))
	}
	return nil
}

func (h Host) run(ctx context.Context, configPath string, herdrArgs []string) (command.Result, error) {
	args := []string{
		"-u", "HERDR_SOCKET_PATH",
		"-u", "HERDR_SESSION",
		"-u", "HERDR_CLIENT_SOCKET_PATH",
		"--", "HERDR_CONFIG_PATH=" + configPath, h.herdr(),
	}
	args = append(args, herdrArgs...)
	return herdrcmd.Run(ctx, h.Runner, environmentBinary, args)
}

func (h Host) validate() (string, error) {
	if h.Runner == nil {
		return "", failure(FailureInvalid, errors.New("command runner is required"), "")
	}
	if h.Herdr != "" && (strings.ContainsRune(h.Herdr, 0) || strings.ContainsRune(h.Herdr, '=')) {
		return "", failure(FailureInvalid, errors.New("Herdr executable contains NUL or equals"), "")
	}
	if h.Session == "default" {
		return "", failure(FailureInvalid, errors.New("the default Herdr session is represented by an empty session name"), "")
	}
	if h.Session != "" {
		if err := herdrid.ValidateSessionName(h.Session); err != nil {
			return "", failure(FailureInvalid, err, "")
		}
	}
	if h.ConfigPath != "" {
		return validateConfigPath(h.ConfigPath)
	}
	return EffectiveConfigPath()
}

func (h Host) herdr() string {
	if h.Herdr == "" {
		return "herdr"
	}
	return h.Herdr
}

func validateConfigPath(path string) (string, error) {
	if len(path) > 4096 || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", failure(FailureInvalid, errors.New("config path must be clean, absolute, NUL-free, and at most 4096 bytes"), "")
	}
	return path, nil
}

func openStore(configPath string) (documentStore, string, error) {
	store, err := documentstore.Open(filepath.Dir(configPath))
	if err != nil {
		return nil, "", err
	}
	return store, filepath.Base(configPath), nil
}

func (h Host) store(configPath string) (documentStore, string, error) {
	if h.openStore != nil {
		return h.openStore(configPath)
	}
	return openStore(configPath)
}

func (h Host) transactionLock(ctx context.Context, configPath string) (io.Closer, error) {
	if h.acquireLock != nil {
		return h.acquireLock(ctx, configPath)
	}
	return acquireTransactionLock(ctx, configPath)
}

func classifyCommandFailure(kind FailureKind, err error, result command.Result) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return failure(FailureCanceled, err, diagnostics(result))
	}
	return failure(kind, err, diagnostics(result))
}

func failure(kind FailureKind, cause error, diagnostics string) error {
	return &Error{Kind: kind, Cause: cause, Diagnostics: diagnostics}
}

func diagnostics(result command.Result) string {
	combined := append([]byte(nil), result.Stdout...)
	combined = append(combined, result.Stderr...)
	return strings.TrimSpace(string(combined))
}

type tomlCommand struct {
	Key         string `toml:"key"`
	Type        string `toml:"type"`
	Command     string `toml:"command"`
	Description string `toml:"description,omitempty"`
}

type configDocument struct {
	Keys struct {
		Command []tomlCommand `toml:"command"`
	} `toml:"keys"`
}

type line struct {
	start   int
	end     int
	content string
}

type ownerGroup struct {
	owner    string
	start    int
	end      int
	bindings []Binding
}

type managedSection struct {
	present bool
	start   int
	end     int
	endLine line
	groups  []ownerGroup
}

func (s managedSection) bindings(owner string) []Binding {
	for _, group := range s.groups {
		if group.owner == owner {
			return group.bindings
		}
	}
	return nil
}

func replaceOwned(data []byte, owner string, desired []Binding) ([]byte, bool, error) {
	section, err := parseManaged(data)
	if err != nil {
		return nil, false, err
	}
	unmanaged := data
	if section.present {
		unmanaged = append(append([]byte(nil), data[:section.start]...), data[section.end:]...)
	}
	existing, err := parseCommands(unmanaged)
	if err != nil {
		return nil, false, failure(FailureCheck, fmt.Errorf("parse unmanaged Herdr config: %w", err), "")
	}
	occupied := make(map[string]string)
	for _, command := range existing {
		if command.Key != "" {
			occupied[command.Key] = "unmanaged config"
		}
	}
	for _, group := range section.groups {
		if group.owner == owner {
			continue
		}
		for _, binding := range group.bindings {
			occupied[binding.Key] = "owner " + group.owner
		}
	}
	for _, binding := range desired {
		if source, ok := occupied[binding.Key]; ok {
			return nil, false, failure(FailureCollision, fmt.Errorf("key %q is already bound by %s", binding.Key, source), "")
		}
	}
	if slices.Equal(section.bindings(owner), desired) {
		return data, false, nil
	}

	groupBytes, err := marshalGroup(owner, desired)
	if err != nil {
		return nil, false, err
	}
	for _, group := range section.groups {
		if group.owner != owner {
			continue
		}
		if len(desired) == 0 && len(section.groups) == 1 {
			next := append(append([]byte(nil), data[:section.start]...), data[section.end:]...)
			return next, true, nil
		}
		var next []byte
		next = append(next, data[:group.start]...)
		next = append(next, groupBytes...)
		next = append(next, data[group.end:]...)
		return next, true, nil
	}
	if len(desired) == 0 {
		return data, false, nil
	}
	if section.present {
		var next []byte
		next = append(next, data[:section.endLine.start]...)
		next = append(next, groupBytes...)
		next = append(next, data[section.endLine.start:]...)
		return next, true, nil
	}
	separator, begin := "", beginNone
	if len(data) > 0 && data[len(data)-1] != '\n' {
		separator, begin = "\n", beginLF
	}
	next := append([]byte(nil), data...)
	next = append(next, separator...)
	next = append(next, begin...)
	next = append(next, '\n')
	next = append(next, groupBytes...)
	next = append(next, endMarker...)
	next = append(next, '\n')
	return next, true, nil
}

func parseManaged(data []byte) (managedSection, error) {
	lines := splitLines(data)
	commentOffsets := tomlCommentOffsets(data)
	beginIndex, endIndex := -1, -1
	separator := 0
	for i, current := range lines {
		if _, isComment := commentOffsets[current.start]; !isComment {
			continue
		}
		if !strings.HasPrefix(current.content, markerPrefix) {
			continue
		}
		switch current.content {
		case beginNone:
			if beginIndex != -1 {
				return managedSection{}, failure(FailureOwnership, errors.New("duplicate or nested managed section"), "")
			}
			beginIndex = i
		case beginLF:
			if beginIndex != -1 {
				return managedSection{}, failure(FailureOwnership, errors.New("duplicate or nested managed section"), "")
			}
			beginIndex = i
			separator = 1
		case endMarker:
			if beginIndex == -1 || endIndex != -1 {
				return managedSection{}, failure(FailureOwnership, errors.New("unmatched managed section end"), "")
			}
			endIndex = i
		default:
			if !strings.HasPrefix(current.content, ownerPrefix) {
				return managedSection{}, failure(FailureOwnership, fmt.Errorf("unsupported managed marker %q", current.content), "")
			}
			if beginIndex == -1 || endIndex != -1 {
				return managedSection{}, failure(FailureOwnership, errors.New("owner marker outside managed section"), "")
			}
		}
	}
	if beginIndex == -1 {
		return managedSection{}, nil
	}
	if endIndex == -1 {
		return managedSection{}, failure(FailureOwnership, errors.New("incomplete managed section"), "")
	}
	beginLine, endLine := lines[beginIndex], lines[endIndex]
	if data[beginLine.end-1] != '\n' || data[endLine.end-1] != '\n' {
		return managedSection{}, failure(FailureOwnership, errors.New("managed section markers must end with LF"), "")
	}
	start := beginLine.start
	if separator == 1 {
		if start == 0 {
			return managedSection{}, failure(FailureOwnership, errors.New("managed separator is missing"), "")
		}
		start--
	}
	section := managedSection{present: true, start: start, end: endLine.end, endLine: endLine}
	seen := map[string]bool{}
	for i := beginIndex + 1; i < endIndex; {
		current := lines[i]
		if !strings.HasPrefix(current.content, ownerPrefix) {
			return managedSection{}, failure(FailureOwnership, errors.New("non-owner content in managed section"), "")
		}
		owner := strings.TrimPrefix(current.content, ownerPrefix)
		if err := validateOwner(owner); err != nil || seen[owner] {
			return managedSection{}, failure(FailureOwnership, fmt.Errorf("invalid or duplicate owner %q", owner), "")
		}
		seen[owner] = true
		groupStart := current.start
		i++
		for i < endIndex && !strings.HasPrefix(lines[i].content, ownerPrefix) {
			if strings.HasPrefix(lines[i].content, markerPrefix) {
				return managedSection{}, failure(FailureOwnership, errors.New("unexpected managed marker"), "")
			}
			i++
		}
		groupEnd := lines[i].start
		payloadStart := current.end
		bindings, err := parseManagedCommands(data[payloadStart:groupEnd], owner)
		if err != nil {
			return managedSection{}, err
		}
		canonical, err := marshalGroup(owner, bindings)
		if err != nil || !bytes.Equal(canonical, data[groupStart:groupEnd]) {
			return managedSection{}, failure(FailureOwnership, fmt.Errorf("managed owner %q bytes are not canonical", owner), "")
		}
		section.groups = append(section.groups, ownerGroup{owner: owner, start: groupStart, end: groupEnd, bindings: bindings})
	}
	if len(section.groups) == 0 {
		return managedSection{}, failure(FailureOwnership, errors.New("managed section has no owners"), "")
	}
	return section, nil
}

func tomlCommentOffsets(data []byte) map[int]struct{} {
	offsets := make(map[int]struct{})
	var parser unstable.Parser
	parser.KeepComments = true
	parser.Reset(data)
	for parser.NextExpression() {
		node := parser.Expression()
		if node.Kind == unstable.Comment {
			offsets[int(node.Raw.Offset)] = struct{}{}
		}
	}
	return offsets
}

func splitLines(data []byte) []line {
	var lines []line
	for start := 0; start < len(data); {
		end := bytes.IndexByte(data[start:], '\n')
		if end < 0 {
			end = len(data)
		} else {
			end += start + 1
		}
		contentEnd := end
		if data[contentEnd-1] == '\n' {
			contentEnd--
		}
		if contentEnd > start && data[contentEnd-1] == '\r' {
			contentEnd--
		}
		lines = append(lines, line{start: start, end: end, content: string(data[start:contentEnd])})
		start = end
	}
	return lines
}

func parseManagedCommands(data []byte, owner string) ([]Binding, error) {
	commands, err := parseCommands(data)
	if err != nil {
		return nil, failure(FailureOwnership, fmt.Errorf("parse owner %q: %w", owner, err), "")
	}
	bindings := make([]Binding, len(commands))
	for i, command := range commands {
		if command.Type != "plugin_action" {
			return nil, failure(FailureOwnership, fmt.Errorf("owner %q command %d is not a plugin action", owner, i), "")
		}
		bindings[i] = Binding{Key: command.Key, Action: command.Command, Description: command.Description}
	}
	if err := validateBindings(owner, bindings); err != nil {
		return nil, failure(FailureOwnership, err, "")
	}
	return bindings, nil
}

func parseCommands(data []byte) ([]tomlCommand, error) {
	var document configDocument
	if err := toml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	return document.Keys.Command, nil
}

func marshalGroup(owner string, bindings []Binding) ([]byte, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	var result bytes.Buffer
	result.WriteString(ownerPrefix)
	result.WriteString(owner)
	result.WriteByte('\n')
	for _, binding := range bindings {
		payload, err := toml.Marshal(tomlCommand{Key: binding.Key, Type: "plugin_action", Command: binding.Action, Description: binding.Description})
		if err != nil {
			return nil, failure(FailureInvalid, fmt.Errorf("marshal binding: %w", err), "")
		}
		result.WriteString("[[keys.command]]\n")
		result.Write(payload)
	}
	return result.Bytes(), nil
}

func validateOwner(owner string) error {
	if err := manifest.ValidateID("owner", owner); err != nil {
		return err
	}
	return nil
}

func validateBindings(owner string, bindings []Binding) error {
	if len(bindings) > MaxBindings {
		return failure(FailureInvalid, fmt.Errorf("bindings exceeds %d entries", MaxBindings), "")
	}
	keys := map[string]bool{}
	for i, binding := range bindings {
		if binding.Key == "" || len(binding.Key) > MaxKeyBytes || strings.ContainsRune(binding.Key, 0) || strings.ContainsAny(binding.Key, "\r\n") || !utf8.ValidString(binding.Key) {
			return failure(FailureInvalid, fmt.Errorf("bindings[%d].key is invalid", i), "")
		}
		if keys[binding.Key] {
			return failure(FailureCollision, fmt.Errorf("duplicate requested key %q", binding.Key), "")
		}
		keys[binding.Key] = true
		if err := manifest.ValidateID("action", binding.Action); err != nil || !strings.HasPrefix(binding.Action, owner+".") {
			return failure(FailureInvalid, fmt.Errorf("bindings[%d].action must be owned by %q", i, owner), "")
		}
		if len(binding.Description) > MaxDescription || strings.ContainsRune(binding.Description, 0) || !utf8.ValidString(binding.Description) {
			return failure(FailureInvalid, fmt.Errorf("bindings[%d].description is invalid", i), "")
		}
	}
	return nil
}
