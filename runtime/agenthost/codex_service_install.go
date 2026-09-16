package agenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	codexServiceUnitName          = "herdr-plugin-kit-codex.service"
	codexServiceDescriptorName    = "codex-service.json"
	codexServiceDescriptorVersion = 1
)

type codexServicePaths struct {
	UnitName             string
	RuntimeDirectoryName string
	ConfigHome           string
	KitConfigDir         string
	UnitDirectory        string
	DescriptorPath       string
	UnitPath             string
	RuntimeDir           string
	SocketPath           string
}

type codexServiceDescriptor struct {
	SchemaVersion     int                            `json:"schemaVersion"`
	UnitName          string                         `json:"unitName"`
	UnitPath          string                         `json:"unitPath"`
	Executable        string                         `json:"executable"`
	CodexHome         string                         `json:"codexHome"`
	SocketPath        string                         `json:"socketPath"`
	RuntimeDirectory  string                         `json:"runtimeDirectory"`
	Version           string                         `json:"version"`
	PendingTransition *codexServicePendingTransition `json:"pendingTransition,omitempty"`
}

// codexServicePendingTransition records the last fully validated process
// while a new unit configuration waits for an explicitly confirmed restart.
// It deliberately lives in the owned descriptor rather than creating a
// general migration store or another mutable service artifact.
type codexServicePendingTransition struct {
	Previous     codexServiceLaunchConfiguration `json:"previous"`
	MainPID      int                             `json:"mainPid"`
	InvocationID string                          `json:"invocationId"`
}

type codexServiceLaunchConfiguration struct {
	Executable       string `json:"executable"`
	CodexHome        string `json:"codexHome"`
	SocketPath       string `json:"socketPath"`
	RuntimeDirectory string `json:"runtimeDirectory"`
	Version          string `json:"version"`
}

func (d codexServiceDescriptor) launchConfiguration() codexServiceLaunchConfiguration {
	return codexServiceLaunchConfiguration{
		Executable: d.Executable, CodexHome: d.CodexHome, SocketPath: d.SocketPath,
		RuntimeDirectory: d.RuntimeDirectory, Version: d.Version,
	}
}

func sameCodexServiceLaunchConfiguration(left, right codexServiceLaunchConfiguration) bool {
	return left.Executable == right.Executable && left.CodexHome == right.CodexHome && left.SocketPath == right.SocketPath && left.RuntimeDirectory == right.RuntimeDirectory && left.Version == right.Version
}

func (p codexServicePendingTransition) previousDescriptor(current codexServiceDescriptor) codexServiceDescriptor {
	current.Executable = p.Previous.Executable
	current.CodexHome = p.Previous.CodexHome
	current.SocketPath = p.Previous.SocketPath
	current.RuntimeDirectory = p.Previous.RuntimeDirectory
	current.Version = p.Previous.Version
	current.PendingTransition = nil
	return current
}

func newCodexServicePendingTransition(previous codexServiceDescriptor, snapshot CodexServiceSnapshot) *codexServicePendingTransition {
	return &codexServicePendingTransition{
		Previous: previous.launchConfiguration(), MainPID: snapshot.MainPID, InvocationID: snapshot.InvocationID,
	}
}

func validCodexServiceLaunchConfiguration(configuration codexServiceLaunchConfiguration) bool {
	return configuration.Version == SupportedCodexVersion && cleanAbsolute(configuration.Executable) && cleanAbsolute(configuration.CodexHome) && cleanAbsolute(configuration.SocketPath) && validRuntimeDirectoryName(configuration.RuntimeDirectory)
}

func validCodexServicePendingTransition(pending *codexServicePendingTransition, current codexServiceDescriptor) bool {
	if pending == nil {
		return true
	}
	if pending.MainPID <= 0 || !validInvocationID(pending.InvocationID) || !validCodexServiceLaunchConfiguration(pending.Previous) {
		return false
	}
	if pending.Previous.SocketPath != current.SocketPath || pending.Previous.RuntimeDirectory != current.RuntimeDirectory || pending.Previous.Version != current.Version {
		return false
	}
	return !sameCodexServiceLaunchConfiguration(pending.Previous, current.launchConfiguration())
}

// CodexServiceInstallOptions selects the one supported executable and home.
// Explicit paths must be absolute; omitted values use the installed Codex
// binary under the effective existing CODEX_HOME.
type CodexServiceInstallOptions struct {
	Executable string
	CodexHome  string
}

// CodexServiceInstallResult reports only Kit-owned paths. It intentionally
// does not enumerate CODEX_HOME, credential, history, or unrelated resources.
type CodexServiceInstallResult struct {
	UnitPath        string
	DescriptorPath  string
	Executable      string
	CodexHome       string
	Changed         bool
	RestartRequired bool
}

// CodexServiceUninstallResult makes a partial maintenance result retryable
// without claiming that unrelated Codex data was removed.
type CodexServiceUninstallResult struct {
	UnitPath          string
	DescriptorPath    string
	RemovedUnit       bool
	RemovedDescriptor bool
	AlreadyAbsent     bool
	Retained          []string
}

func defaultCodexServicePaths() (codexServicePaths, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return codexServicePaths{}, err
		}
		configHome = filepath.Join(home, ".config")
	}
	if !cleanAbsolute(configHome) {
		return codexServicePaths{}, errors.New("XDG_CONFIG_HOME must be an absolute clean path")
	}
	runtimeRoot := os.Getenv("XDG_RUNTIME_DIR")
	if !cleanAbsolute(runtimeRoot) {
		return codexServicePaths{}, errors.New("XDG_RUNTIME_DIR must be an absolute clean path")
	}
	runtimeInfo, err := os.Lstat(runtimeRoot)
	if err != nil || runtimeInfo.Mode()&os.ModeSymlink != 0 || !runtimeInfo.IsDir() {
		return codexServicePaths{}, errors.New("XDG_RUNTIME_DIR must be an existing non-symlink directory")
	}
	configHome = filepath.Clean(configHome)
	runtimeRoot = filepath.Clean(runtimeRoot)
	kitConfigDir := filepath.Join(configHome, "herdr-plugin-kit")
	unitDirectory := filepath.Join(configHome, "systemd", "user")
	runtimeDir := filepath.Join(runtimeRoot, "herdr-plugin-kit")
	return codexServicePaths{
		UnitName: codexServiceUnitName, RuntimeDirectoryName: "herdr-plugin-kit",
		ConfigHome: configHome, KitConfigDir: kitConfigDir, UnitDirectory: unitDirectory,
		DescriptorPath: filepath.Join(kitConfigDir, codexServiceDescriptorName),
		UnitPath:       filepath.Join(unitDirectory, codexServiceUnitName),
		RuntimeDir:     runtimeDir,
		SocketPath:     filepath.Join(runtimeDir, "codex.sock"),
	}, nil
}

func (p codexServicePaths) unitName() string {
	if p.UnitName == "" {
		return codexServiceUnitName
	}
	return p.UnitName
}

func (p codexServicePaths) runtimeDirectoryName() string {
	if p.RuntimeDirectoryName == "" {
		return "herdr-plugin-kit"
	}
	return p.RuntimeDirectoryName
}

// Install writes and validates the owned descriptor and unit but never starts
// or restarts a client. Start is an explicit separate command.
func (s *CodexService) Install(ctx context.Context, options CodexServiceInstallOptions) (CodexServiceInstallResult, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return CodexServiceInstallResult{}, errors.Join(ErrCodexServiceUnavailable, err)
	}
	if !s.usable() {
		return CodexServiceInstallResult{}, ErrCodexServiceUnavailable
	}
	bounds, cancel := s.withTimeout(ctx)
	defer cancel()
	paths, err := s.resolvePaths()
	if err != nil {
		return CodexServiceInstallResult{}, fmt.Errorf("%w: service paths: %v", ErrCodexServiceDrift, err)
	}
	home, err := s.installHome(options.CodexHome)
	if err != nil {
		return CodexServiceInstallResult{}, fmt.Errorf("%w: CODEX_HOME: %v", ErrCodexServiceDrift, err)
	}
	active, err := s.nativeDaemon(bounds, home)
	if err != nil {
		return CodexServiceInstallResult{}, errors.Join(ErrCodexServiceUnavailable, err)
	}
	if active {
		return CodexServiceInstallResult{}, fmt.Errorf("%w: canonical native daemon is listening for %q; settle clients and migrate it explicitly", ErrCodexServiceMigrationRequired, home)
	}
	executable, err := s.installExecutable(options.Executable, home)
	if err != nil {
		return CodexServiceInstallResult{}, fmt.Errorf("%w: executable: %v", ErrCodexServiceIncompatible, err)
	}
	if err := s.validateExecutableVersion(bounds, executable); err != nil {
		return CodexServiceInstallResult{}, err
	}
	if err := s.requireUserManager(bounds); err != nil {
		return CodexServiceInstallResult{}, err
	}
	lock, err := s.lifecycleLock(bounds, paths, true)
	if err != nil {
		return CodexServiceInstallResult{}, err
	}
	defer lock.Close()

	descriptor := codexServiceDescriptor{
		SchemaVersion: codexServiceDescriptorVersion, UnitName: paths.unitName(), UnitPath: paths.UnitPath,
		Executable: executable, CodexHome: home, SocketPath: paths.SocketPath, RuntimeDirectory: paths.runtimeDirectoryName(), Version: SupportedCodexVersion,
	}
	state, err := inspectCodexServiceInstallation(paths)
	if err != nil {
		return CodexServiceInstallResult{}, err
	}
	if err := s.rejectForeignLoadedUnit(bounds, paths, descriptor.UnitName); err != nil {
		return CodexServiceInstallResult{}, err
	}
	descriptor, err = s.descriptorForInstallStage(bounds, paths, state, descriptor)
	if err != nil {
		return CodexServiceInstallResult{}, err
	}
	descriptorBytes, err := marshalCodexServiceDescriptor(descriptor)
	if err != nil {
		return CodexServiceInstallResult{}, err
	}
	unitBytes, err := renderCodexServiceUnit(descriptor)
	if err != nil {
		return CodexServiceInstallResult{}, err
	}
	changed := !state.unitExact(unitBytes) || !state.descriptorExact(descriptorBytes)
	configurationChanged := state.unitExists && !sameCodexServiceLaunchConfiguration(state.descriptor.launchConfiguration(), descriptor.launchConfiguration())
	if changed {
		if err := atomicOwnedWrite(paths.UnitPath, unitBytes, 0o600); err != nil {
			return CodexServiceInstallResult{}, err
		}
		if err := s.verifyUnit(bounds, paths.UnitPath); err != nil {
			return CodexServiceInstallResult{}, errors.Join(err, restoreOwnedUnit(paths.UnitPath, unitBytes, state.unitBytes, state.unitExists))
		}
		if err := atomicOwnedWrite(paths.DescriptorPath, descriptorBytes, 0o600); err != nil {
			return CodexServiceInstallResult{}, errors.Join(err, restoreOwnedUnit(paths.UnitPath, unitBytes, state.unitBytes, state.unitExists))
		}
	} else if err := s.verifyUnit(bounds, paths.UnitPath); err != nil {
		return CodexServiceInstallResult{}, err
	}
	if err := s.daemonReload(bounds); err != nil {
		return CodexServiceInstallResult{}, err
	}
	if err := s.rejectForeignLoadedUnit(bounds, paths, descriptor.UnitName); err != nil {
		return CodexServiceInstallResult{}, err
	}
	return CodexServiceInstallResult{
		UnitPath: paths.UnitPath, DescriptorPath: paths.DescriptorPath, Executable: executable, CodexHome: home,
		Changed: changed, RestartRequired: configurationChanged || descriptor.PendingTransition != nil,
	}, nil
}

// Start enables the owned unit for boot persistence and starts it. It refuses
// coexistence with the configured home's canonical native daemon.
func (s *CodexService) Start(ctx context.Context) (CodexServiceSnapshot, error) {
	return s.startOrRestart(ctx, false, false)
}

// Restart is disruptive maintenance. The caller must explicitly confirm that
// managed clients will disconnect and may have unsaved drafts.
func (s *CodexService) Restart(ctx context.Context, confirmDisconnect bool) (CodexServiceSnapshot, error) {
	return s.startOrRestart(ctx, true, confirmDisconnect)
}

func (s *CodexService) startOrRestart(ctx context.Context, restart, confirmDisconnect bool) (CodexServiceSnapshot, error) {
	if ctx == nil {
		panic("nil context")
	}
	if restart && !confirmDisconnect {
		return CodexServiceSnapshot{State: CodexServiceDegraded}, ErrCodexServiceConfirmationRequired
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
	paths, descriptor, snapshot, err := s.runtimeConfiguration(bounds)
	if err != nil {
		return snapshot, err
	}
	if err := s.requireUserManager(bounds); err != nil {
		return snapshot, err
	}
	if err := s.verifyOwnedUnitInventory(bounds, paths, descriptor); err != nil {
		return snapshot, err
	}
	if !restart {
		if _, err := s.runner.Run(bounds, "systemctl", []string{"--user", "enable", descriptor.UnitName}); err != nil {
			return snapshot, errors.Join(ErrCodexServiceUnavailable, err)
		}
		ready, show, inspectErr := s.inspectConfigured(bounds, paths, descriptor, snapshot)
		if inspectErr == nil {
			return s.finalizeReadyTransition(paths, descriptor, ready)
		}
		if !errors.Is(inspectErr, ErrCodexServiceUnavailable) || !show.startable() {
			return ready, inspectErr
		}
	}
	if err := s.validateExecutableVersion(bounds, descriptor.Executable); err != nil {
		return snapshot, err
	}
	verb := "start"
	if restart {
		verb = "restart"
	}
	if _, err := s.runner.Run(bounds, "systemctl", []string{"--user", verb, descriptor.UnitName}); err != nil {
		return snapshot, errors.Join(ErrCodexServiceUnavailable, err)
	}
	return s.awaitReadyAndFinalizeTransition(bounds, paths, descriptor, snapshot)
}

// Uninstall removes only exact, owned service files. It is deliberately
// disruptive maintenance and never attempts a no-client admission protocol.
func (s *CodexService) Uninstall(ctx context.Context, confirmDisconnect bool) (CodexServiceUninstallResult, error) {
	if ctx == nil {
		panic("nil context")
	}
	if !confirmDisconnect {
		return CodexServiceUninstallResult{}, ErrCodexServiceConfirmationRequired
	}
	if err := ctx.Err(); err != nil {
		return CodexServiceUninstallResult{}, errors.Join(ErrCodexServiceUnavailable, err)
	}
	if !s.usable() {
		return CodexServiceUninstallResult{}, ErrCodexServiceUnavailable
	}
	bounds, cancel := s.withTimeout(ctx)
	defer cancel()
	paths, err := s.resolvePaths()
	if err != nil {
		return CodexServiceUninstallResult{}, fmt.Errorf("%w: service paths: %v", ErrCodexServiceDrift, err)
	}
	result := CodexServiceUninstallResult{
		UnitPath: paths.UnitPath, DescriptorPath: paths.DescriptorPath,
		Retained: []string{"Codex executable", "CODEX_HOME data", "linger and user manager", "unrelated services and SSH configuration", "Kit lifecycle lock"},
	}
	lock, lockErr := s.lifecycleLock(bounds, paths, false)
	if lockErr != nil {
		if errors.Is(lockErr, ErrCodexServiceNotInstalled) && s.afterLifecycleLockMiss != nil {
			s.afterLifecycleLockMiss()
		}
		state, inspectErr := inspectCodexServiceInstallation(paths)
		if inspectErr != nil {
			return result, inspectErr
		}
		if !state.descriptorExists && errors.Is(lockErr, ErrCodexServiceNotInstalled) {
			result.AlreadyAbsent = true
			return result, nil
		}
		if !state.descriptorExists {
			return result, fmt.Errorf("%w: unit exists without owned descriptor", ErrCodexServiceOwnership)
		}
		return result, errors.Join(ErrCodexServiceUnavailable, lockErr)
	}
	defer lock.Close()
	state, err := inspectCodexServiceInstallation(paths)
	if err != nil {
		return result, err
	}
	if !state.descriptorExists {
		return result, fmt.Errorf("%w: owned descriptor disappeared during maintenance", ErrCodexServiceOwnership)
	}
	if err := s.requireUserManager(bounds); err != nil {
		return result, err
	}
	if state.unitExists {
		if err := s.verifyOwnedUnitInventory(bounds, paths, state.descriptor); err != nil {
			return result, err
		}
		if _, err := s.runner.Run(bounds, "systemctl", []string{"--user", "disable", state.descriptor.UnitName}); err != nil {
			return result, errors.Join(ErrCodexServiceUnavailable, err)
		}
		if _, err := s.runner.Run(bounds, "systemctl", []string{"--user", "stop", state.descriptor.UnitName}); err != nil {
			return result, errors.Join(ErrCodexServiceUnavailable, err)
		}
		if err := s.verifyStopped(bounds, state.descriptor.UnitName); err != nil {
			return result, err
		}
		if err := removeExactOwnedFile(paths.UnitPath, state.unitBytes); err != nil {
			return result, err
		}
		result.RemovedUnit = true
	}
	if err := s.daemonReload(bounds); err != nil {
		return result, err
	}
	if err := s.verifyUninstalled(bounds, state.descriptor.UnitName); err != nil {
		return result, err
	}
	if err := removeExactOwnedFile(paths.DescriptorPath, state.descriptorBytes); err != nil {
		return result, err
	}
	result.RemovedDescriptor = true
	return result, nil
}

func (s *CodexService) verifyOwnedUnitInventory(ctx context.Context, paths codexServicePaths, descriptor codexServiceDescriptor) error {
	show, err := s.showUnit(ctx, descriptor.UnitName)
	if err != nil {
		return err
	}
	if show.LoadState == "not-found" {
		return ErrCodexServiceNotInstalled
	}
	if !loadedCodexServiceUnitMatches(paths, descriptor, show) {
		return fmt.Errorf("%w: loaded unit does not match owned configuration", ErrCodexServiceOwnership)
	}
	if show.ActiveState != "active" {
		return nil
	}
	if show.SubState != "running" || show.MainPID <= 0 || !validInvocationID(show.InvocationID) {
		return fmt.Errorf("%w: state=%s/%s pid=%d", ErrCodexServiceUnavailable, show.ActiveState, show.SubState, show.MainPID)
	}
	if desiredErr := s.validateProcess(descriptor, show); desiredErr == nil {
		return nil
	} else if descriptor.PendingTransition == nil {
		return desiredErr
	}
	if receiptErr := s.validateReceiptBoundPreviousProcess(descriptor, show); receiptErr != nil {
		return receiptErr
	}
	return nil
}

// validateReceiptBoundPreviousProcess is the maintenance-side counterpart to
// inspectReceiptBoundPrevious. Restart and Uninstall need exact unit/process
// ownership before they disrupt a process, but must not require a currently
// responsive socket or protocol server in order to recover or stop it.
func (s *CodexService) validateReceiptBoundPreviousProcess(descriptor codexServiceDescriptor, show codexSystemdShow) error {
	pending := descriptor.PendingTransition
	if pending == nil {
		return fmt.Errorf("%w: no pending transition", ErrCodexServiceDrift)
	}
	if show.MainPID != pending.MainPID || show.InvocationID != pending.InvocationID {
		return fmt.Errorf("%w: pending transition process identity", ErrCodexServiceDrift)
	}
	return s.validateProcess(pending.previousDescriptor(descriptor), show)
}

// finalizeReadyTransition removes a transition receipt only after the caller
// has already completed the desired process, socket, and protocol checks. If
// cleanup cannot prove the exact descriptor is still ours, it leaves the
// receipt inert and returns the ready snapshot with the truthful error.
func (s *CodexService) finalizeReadyTransition(paths codexServicePaths, descriptor codexServiceDescriptor, ready CodexServiceSnapshot) (CodexServiceSnapshot, error) {
	if descriptor.PendingTransition == nil {
		return ready, nil
	}
	current, err := marshalCodexServiceDescriptor(descriptor)
	if err != nil {
		return ready, err
	}
	stored, exists, err := readRegularOwnedFile(paths.DescriptorPath)
	if err != nil {
		return ready, err
	}
	if !exists || !bytes.Equal(stored, current) {
		return ready, fmt.Errorf("%w: descriptor changed before transition cleanup", ErrCodexServiceOwnership)
	}
	descriptor.PendingTransition = nil
	cleared, err := marshalCodexServiceDescriptor(descriptor)
	if err != nil {
		return ready, err
	}
	if err := atomicOwnedWrite(paths.DescriptorPath, cleared, 0o600); err != nil {
		return ready, err
	}
	return ready, nil
}

// rejectForeignLoadedUnit runs before Install writes or reloads a same-named
// unit. A manager-loaded fragment outside the exact owned path may be an
// administrator, transient, or other application's unit and is never
// shadowed by Kit installation.
func (s *CodexService) rejectForeignLoadedUnit(ctx context.Context, paths codexServicePaths, unit string) error {
	show, err := s.showUnit(ctx, unit)
	if err != nil {
		return err
	}
	if show.LoadState == "not-found" {
		return nil
	}
	if show.LoadState != "loaded" || show.FragmentPath != paths.UnitPath {
		return fmt.Errorf("%w: same-named unit is already loaded from %q", ErrCodexServiceOwnership, show.FragmentPath)
	}
	return nil
}

// descriptorForInstallStage preserves a verified running source while a new
// unit is staged. A later Install must never recast an unstarted desired
// configuration as the source: only the current desired process, or the exact
// PID/invocation recorded in an existing receipt, can supply the transition.
func (s *CodexService) descriptorForInstallStage(ctx context.Context, paths codexServicePaths, state codexServiceInstallationState, desired codexServiceDescriptor) (codexServiceDescriptor, error) {
	if !state.descriptorExists {
		return desired, nil
	}
	current := state.descriptor
	show, err := s.showUnit(ctx, current.UnitName)
	if err != nil {
		return desired, err
	}
	if show.LoadState == "not-found" {
		// No process can be manager-owned while the unit is absent. Retain a
		// previous receipt inertly so a later Start/Restart can still reach the
		// desired configuration without ever treating the source as ready.
		desired.PendingTransition = retainedCodexServicePendingTransition(current, desired)
		return desired, nil
	}
	if !loadedCodexServiceUnitMatches(paths, current, show) {
		return desired, fmt.Errorf("%w: loaded unit does not match owned configuration", ErrCodexServiceOwnership)
	}
	if show.ActiveState != "active" {
		desired.PendingTransition = retainedCodexServicePendingTransition(current, desired)
		return desired, nil
	}
	if show.SubState != "running" || show.MainPID <= 0 || !validInvocationID(show.InvocationID) {
		return desired, fmt.Errorf("%w: state=%s/%s pid=%d", ErrCodexServiceUnavailable, show.ActiveState, show.SubState, show.MainPID)
	}

	snapshot := CodexServiceSnapshot{State: CodexServiceDegraded, CodexHome: current.CodexHome, SocketPath: current.SocketPath}
	if ready, currentErr := s.inspectRunningConfiguration(ctx, paths, current, snapshot, show); currentErr == nil {
		if sameCodexServiceLaunchConfiguration(current.launchConfiguration(), desired.launchConfiguration()) {
			// A naturally restarted desired process is fully verified here. Its
			// stale receipt is no longer needed and can be cleared by this owned
			// Install operation.
			desired.PendingTransition = nil
			return desired, nil
		}
		desired.PendingTransition = newCodexServicePendingTransition(current, ready)
		return desired, nil
	} else if current.PendingTransition == nil {
		return desired, currentErr
	}

	if _, receiptErr := s.inspectReceiptBoundPrevious(ctx, paths, current, snapshot, show); receiptErr != nil {
		return desired, receiptErr
	}
	previous := current.PendingTransition.previousDescriptor(current)
	if sameCodexServiceLaunchConfiguration(previous.launchConfiguration(), desired.launchConfiguration()) {
		// Restoring the actual receipt-bound configuration makes it desired
		// again, so retaining the receipt would be both redundant and invalid.
		desired.PendingTransition = nil
		return desired, nil
	}
	desired.PendingTransition = current.PendingTransition
	return desired, nil
}

func retainedCodexServicePendingTransition(current, desired codexServiceDescriptor) *codexServicePendingTransition {
	pending := current.PendingTransition
	if pending != nil && sameCodexServiceLaunchConfiguration(pending.previousDescriptor(current).launchConfiguration(), desired.launchConfiguration()) {
		return nil
	}
	return pending
}

func systemdExecStartMatches(value string, descriptor codexServiceDescriptor) bool {
	const pathPrefix = "{ path="
	const argvPrefix = " ; argv[]="
	_, rest, found := strings.Cut(value, pathPrefix)
	if !found {
		return false
	}
	path, rest, found := strings.Cut(rest, argvPrefix)
	if !found {
		return false
	}
	argv, _, found := strings.Cut(rest, " ; ")
	if !found {
		return false
	}
	return path == descriptor.Executable && argv == descriptor.Executable+" app-server --listen unix://"+descriptor.SocketPath
}

func (s *CodexService) installHome(explicit string) (string, error) {
	if explicit != "" {
		return canonicalExistingDirectory(explicit, true)
	}
	return s.resolveHome()
}

func (s *CodexService) installExecutable(explicit, home string) (string, error) {
	path := explicit
	if path == "" {
		path = filepath.Join(home, "packages", "standalone", "current", "bin", "codex")
	} else if !cleanAbsolute(path) {
		return "", errors.New("--codex must be an absolute clean path")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("Codex executable is not an executable regular file")
	}
	return filepath.Clean(canonical), nil
}

func (s *CodexService) validateExecutableVersion(ctx context.Context, executable string) error {
	result, err := s.runner.Run(ctx, executable, []string{"--version"})
	if err != nil || result.StdoutTruncated || result.StderrTruncated {
		return errors.Join(ErrCodexServiceIncompatible, err)
	}
	for _, field := range strings.Fields(string(result.Stdout)) {
		if strings.Trim(field, "v,;()[]") == SupportedCodexVersion {
			return nil
		}
	}
	return fmt.Errorf("%w: expected Codex %s", ErrCodexServiceIncompatible, SupportedCodexVersion)
}

func (s *CodexService) requireUserManager(ctx context.Context) error {
	if result, err := s.runner.Run(ctx, "systemctl", []string{"--user", "show-environment"}); err != nil || result.StdoutTruncated || result.StderrTruncated {
		return errors.Join(ErrCodexServiceUnavailable, err)
	}
	uid := os.Geteuid()
	if s.euid != nil {
		uid = s.euid()
	}
	result, err := s.runner.Run(ctx, "loginctl", []string{"show-user", strconv.Itoa(uid), "--property=Linger", "--value"})
	if err != nil || result.StdoutTruncated || result.StderrTruncated {
		return errors.Join(ErrCodexServiceUnavailable, err)
	}
	if strings.TrimSpace(strings.ToLower(string(result.Stdout))) != "yes" {
		return fmt.Errorf("%w: user lingering is required for boot persistence", ErrCodexServiceUnavailable)
	}
	return nil
}

func (s *CodexService) verifyUnit(ctx context.Context, unitPath string) error {
	result, err := s.runner.Run(ctx, "systemd-analyze", []string{"verify", unitPath})
	if err != nil || result.StdoutTruncated || result.StderrTruncated {
		return errors.Join(ErrCodexServiceReceipt, err)
	}
	return nil
}

func (s *CodexService) daemonReload(ctx context.Context) error {
	if _, err := s.runner.Run(ctx, "systemctl", []string{"--user", "daemon-reload"}); err != nil {
		return errors.Join(ErrCodexServiceUnavailable, err)
	}
	return nil
}

func (s *CodexService) verifyStopped(ctx context.Context, unit string) error {
	show, err := s.showUnit(ctx, unit)
	if err != nil {
		return err
	}
	if show.LoadState == "not-found" {
		return nil
	}
	if show.ActiveState != "inactive" || show.SubState != "dead" || show.MainPID != 0 {
		return fmt.Errorf("%w: service did not stop (%s/%s pid=%d)", ErrCodexServiceUnavailable, show.ActiveState, show.SubState, show.MainPID)
	}
	return nil
}

func (s *CodexService) verifyUninstalled(ctx context.Context, unit string) error {
	show, err := s.showUnit(ctx, unit)
	if err != nil && !errors.Is(err, ErrCodexServiceNotInstalled) {
		return err
	}
	if show.LoadState != "not-found" || (show.UnitFileState != "" && show.UnitFileState != "disabled" && show.UnitFileState != "not-found") {
		return fmt.Errorf("%w: unit remains loaded or enabled", ErrCodexServiceOwnership)
	}
	result, _ := s.runner.Run(ctx, "systemctl", []string{"--user", "is-enabled", unit})
	state := strings.TrimSpace(string(result.Stdout))
	if result.StdoutTruncated || result.StderrTruncated || (state != "" && state != "disabled" && state != "not-found") {
		return fmt.Errorf("%w: unit remains enabled", ErrCodexServiceOwnership)
	}
	return nil
}

func ensureCodexServiceDirectories(paths codexServicePaths) error {
	if err := ensureNoSymlinkDirectory(paths.ConfigHome, true); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(paths.ConfigHome, "systemd"), paths.UnitDirectory, paths.KitConfigDir} {
		if err := ensureNoSymlinkDirectory(path, true); err != nil {
			return err
		}
	}
	return nil
}

func ensureNoSymlinkDirectory(path string, create bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ownedSafeCodexServiceLifecyclePath(info, os.Geteuid()) {
		return fmt.Errorf("%w: directory %q", ErrCodexServiceOwnership, path)
	}
	return nil
}

func ownedSafeCodexServiceLifecyclePath(info fs.FileInfo, uid int) bool {
	return ownedBy(info, uid) && info.Mode().Perm()&0o022 == 0
}

type codexServiceInstallationState struct {
	descriptor       codexServiceDescriptor
	descriptorBytes  []byte
	unitBytes        []byte
	descriptorExists bool
	unitExists       bool
}

func (s codexServiceInstallationState) unitExact(want []byte) bool {
	return s.unitExists && bytes.Equal(s.unitBytes, want)
}

func (s codexServiceInstallationState) descriptorExact(want []byte) bool {
	return s.descriptorExists && bytes.Equal(s.descriptorBytes, want)
}

func inspectCodexServiceInstallation(paths codexServicePaths) (codexServiceInstallationState, error) {
	if exists, err := pathExists(paths.UnitPath + ".d"); err != nil || exists {
		if err != nil {
			return codexServiceInstallationState{}, err
		}
		return codexServiceInstallationState{}, fmt.Errorf("%w: service drop-ins are not owned", ErrCodexServiceOwnership)
	}
	descriptorBytes, descriptorExists, err := readRegularOwnedFile(paths.DescriptorPath)
	if err != nil {
		return codexServiceInstallationState{}, err
	}
	unitBytes, unitExists, err := readRegularOwnedFile(paths.UnitPath)
	if err != nil {
		return codexServiceInstallationState{}, err
	}
	state := codexServiceInstallationState{descriptorBytes: descriptorBytes, descriptorExists: descriptorExists, unitBytes: unitBytes, unitExists: unitExists}
	if !descriptorExists && !unitExists {
		return state, nil
	}
	if !descriptorExists {
		return codexServiceInstallationState{}, fmt.Errorf("%w: unit exists without descriptor", ErrCodexServiceOwnership)
	}
	descriptor, err := parseOwnedCodexServiceDescriptor(descriptorBytes, paths)
	if err != nil {
		return codexServiceInstallationState{}, err
	}
	state.descriptor = descriptor
	if unitExists {
		want, err := renderCodexServiceUnit(descriptor)
		if err != nil {
			return codexServiceInstallationState{}, err
		}
		if !bytes.Equal(unitBytes, want) {
			return codexServiceInstallationState{}, fmt.Errorf("%w: unit contents changed", ErrCodexServiceOwnership)
		}
	}
	return state, nil
}

func readOwnedCodexService(paths codexServicePaths) (codexServiceDescriptor, error) {
	state, err := inspectCodexServiceInstallation(paths)
	if err != nil {
		return codexServiceDescriptor{}, err
	}
	if !state.descriptorExists || !state.unitExists {
		return codexServiceDescriptor{}, ErrCodexServiceNotInstalled
	}
	return state.descriptor, nil
}

func readRegularOwnedFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !ownedSafeCodexServiceLifecyclePath(info, os.Geteuid()) {
		return nil, false, fmt.Errorf("%w: file %q", ErrCodexServiceOwnership, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxCodexServiceReceiptBytes {
		return nil, false, fmt.Errorf("%w: file %q is too large", ErrCodexServiceOwnership, path)
	}
	return data, true, nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func parseOwnedCodexServiceDescriptor(raw []byte, paths codexServicePaths) (codexServiceDescriptor, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var descriptor codexServiceDescriptor
	if err := decoder.Decode(&descriptor); err != nil {
		return codexServiceDescriptor{}, fmt.Errorf("%w: descriptor: %v", ErrCodexServiceOwnership, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return codexServiceDescriptor{}, fmt.Errorf("%w: descriptor has extra JSON", ErrCodexServiceOwnership)
	}
	canonical, err := marshalCodexServiceDescriptor(descriptor)
	if err != nil || !bytes.Equal(raw, canonical) {
		return codexServiceDescriptor{}, fmt.Errorf("%w: descriptor contents changed", ErrCodexServiceOwnership)
	}
	if descriptor.SchemaVersion != codexServiceDescriptorVersion || descriptor.UnitName != paths.unitName() || descriptor.UnitPath != paths.UnitPath || descriptor.SocketPath != paths.SocketPath || descriptor.RuntimeDirectory != paths.runtimeDirectoryName() || !validCodexServiceLaunchConfiguration(descriptor.launchConfiguration()) || !validCodexServicePendingTransition(descriptor.PendingTransition, descriptor) {
		return codexServiceDescriptor{}, fmt.Errorf("%w: descriptor identity", ErrCodexServiceOwnership)
	}
	return descriptor, nil
}

func marshalCodexServiceDescriptor(descriptor codexServiceDescriptor) ([]byte, error) {
	data, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func renderCodexServiceUnit(descriptor codexServiceDescriptor) ([]byte, error) {
	if !validRuntimeDirectoryName(descriptor.RuntimeDirectory) {
		return nil, errors.New("invalid RuntimeDirectory name")
	}
	executable, err := quoteSystemdValue(descriptor.Executable, false)
	if err != nil {
		return nil, err
	}
	environment, err := quoteSystemdValue("CODEX_HOME="+descriptor.CodexHome, false)
	if err != nil {
		return nil, err
	}
	appServer, err := quoteSystemdValue("app-server", false)
	if err != nil {
		return nil, err
	}
	listenFlag, err := quoteSystemdValue("--listen", false)
	if err != nil {
		return nil, err
	}
	listenSocket, err := quoteSystemdValue("unix://%t/"+descriptor.RuntimeDirectory+"/codex.sock", true)
	if err != nil {
		return nil, err
	}
	unit := "[Unit]\n" +
		"Description=Herdr Plugin Kit shared Codex App Server\n" +
		"StartLimitIntervalSec=60s\n" +
		"StartLimitBurst=3\n\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"Environment=" + environment + "\n" +
		"WorkingDirectory=%h\n" +
		"RuntimeDirectory=" + descriptor.RuntimeDirectory + "\n" +
		"RuntimeDirectoryMode=0700\n" +
		"UMask=0077\n" +
		"ExecStart=" + executable + " " + appServer + " " + listenFlag + " " + listenSocket + "\n" +
		"Restart=on-failure\n" +
		"RestartSec=2s\n" +
		"TimeoutStopSec=10s\n\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
	return []byte(unit), nil
}

func validRuntimeDirectoryName(name string) bool {
	return name != "" && filepath.Base(name) == name && name != "." && name != ".." && !strings.ContainsRune(name, 0) && !strings.ContainsRune(name, '%')
}

func quoteSystemdValue(value string, preserveSpecifiers bool) (string, error) {
	if value == "" || strings.ContainsRune(value, 0) {
		return "", errors.New("systemd value is empty or contains NUL")
	}
	var quoted strings.Builder
	quoted.WriteByte('"')
	for _, char := range value {
		switch char {
		case '\n', '\r':
			return "", errors.New("systemd value contains a newline")
		case '\\':
			quoted.WriteString("\\\\")
		case '"':
			quoted.WriteString("\\\"")
		case '%':
			if preserveSpecifiers {
				quoted.WriteRune(char)
			} else {
				quoted.WriteString("%%")
			}
		default:
			quoted.WriteRune(char)
		}
	}
	quoted.WriteByte('"')
	return quoted.String(), nil
}

func atomicOwnedWrite(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := ensureNoSymlinkDirectory(directory, false); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".herdr-plugin-kit-codex-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func removeExactOwnedFile(path string, want []byte) error {
	got, exists, err := readRegularOwnedFile(path)
	if err != nil {
		return err
	}
	if !exists || !bytes.Equal(got, want) {
		return fmt.Errorf("%w: file changed before removal", ErrCodexServiceOwnership)
	}
	return os.Remove(path)
}

func restoreOwnedUnit(path string, current, previous []byte, previousExists bool) error {
	got, exists, err := readRegularOwnedFile(path)
	if err != nil {
		return err
	}
	if !exists || !bytes.Equal(got, current) {
		return fmt.Errorf("%w: unit changed during rollback", ErrCodexServiceOwnership)
	}
	if previousExists {
		return atomicOwnedWrite(path, previous, 0o600)
	}
	return os.Remove(path)
}
