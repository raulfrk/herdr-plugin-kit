package servicehost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

const (
	lifecycleDescriptorVersion = 1
	DefaultLifecycleTimeout    = 10 * time.Second
	maxLifecycleDescriptor     = 64 << 10
	lifecycleDrainDialTimeout  = 2 * time.Second
	lifecycleReadyDialTimeout  = 250 * time.Millisecond
	lifecycleReadyPollInterval = 25 * time.Millisecond
	maxLifecyclePurgeEntries   = 100000
)

var (
	ErrConsentRequired              = errors.New("servicehost: explicit lifecycle consent required")
	ErrLifecycleBusy                = errors.New("servicehost: lifecycle operation already in progress")
	ErrLifecycleOwnership           = errors.New("servicehost: lifecycle ownership mismatch")
	ErrUpgradeRequired              = errors.New("servicehost: installed service differs; staged upgrade required")
	ErrUpgradeRolledBack            = errors.New("servicehost: upgrade failed and was rolled back")
	ErrUpgradeRecoveryRequired      = errors.New("servicehost: upgrade recovery required")
	ErrPurgeRecoveryConsentRequired = errors.New("servicehost: explicit recovery-data loss confirmation required")
	ErrNotInstalled                 = errors.New("servicehost: service is not installed")
	ErrLifecycleUnavailable         = errors.New("servicehost: lifecycle manager unavailable")
)

type LifecyclePaths struct {
	UnitPath             string
	DescriptorPath       string
	BinaryDirectory      string
	RuntimeDirectory     string
	RuntimeDirectoryName string
	SocketPath           string
	PurgePaths           []string
}

type LifecycleConfig struct {
	UnitName         string
	ServiceName      string
	Description      string
	ProtocolVersion  uint32
	ClientName       string
	Paths            LifecyclePaths
	CommandTimeout   time.Duration
	ReadinessTimeout time.Duration
	DrainTimeout     time.Duration
}

type InstallRequest struct {
	Consent          bool
	SourceExecutable string
	Version          string
	Arguments        []string
}

type InstallResult struct {
	UnitPath       string
	DescriptorPath string
	Executable     string
	Version        string
	Changed        bool
}

type UpgradeStageResult struct {
	CurrentVersion string
	PendingVersion string
	Executable     string
	Changed        bool
}

type UpgradeRelease struct {
	Executable       string
	ExecutableSHA256 string
	Version          string
	Arguments        []string
}

type RollbackCompatibilityCheck func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error)

type UpgradeActivationRequest struct {
	Confirm       bool
	RollbackCheck RollbackCompatibilityCheck
}

type UpgradeActivationResult struct {
	Status           LifecycleStatus
	CurrentVersion   string
	PreviousVersion  string
	RolledBack       bool
	RecoveryRequired bool
}

type UninstallRequest struct {
	Confirm bool
}

type UninstallResult struct {
	UnitPath           string
	DescriptorPath     string
	RemovedUnit        bool
	RemovedDescriptor  bool
	RemovedSocket      bool
	RemovedExecutables []string
	AlreadyAbsent      bool
	Retained           []string
}

type PurgeRequest struct {
	Confirm             bool
	ConfirmRecoveryLoss bool
}

type PurgeResult struct {
	RemovedPaths    []string
	RemovedManifest bool
	AlreadyAbsent   bool
}

type Lifecycle struct {
	config                LifecycleConfig
	runner                command.Runner
	afterDrainResponse    func() error
	upgradeCleanupTimeout func() time.Duration
}

type LifecycleStatus struct {
	Installed     bool
	Enabled       bool
	Active        bool
	UnitName      string
	LoadState     string
	ActiveState   string
	SubState      string
	Result        string
	MainPID       int
	FragmentPath  string
	UnitFileState string
}

type lifecycleDescriptor struct {
	SchemaVersion        int                  `json:"schemaVersion"`
	UnitName             string               `json:"unitName"`
	ServiceName          string               `json:"serviceName"`
	UnitPath             string               `json:"unitPath"`
	Executable           string               `json:"executable"`
	ExecutableSHA256     string               `json:"executableSHA256"`
	Version              string               `json:"version"`
	Arguments            []string             `json:"arguments"`
	ProtocolVersion      uint32               `json:"protocolVersion"`
	RuntimeDirectory     string               `json:"runtimeDirectory"`
	RuntimeDirectoryName string               `json:"runtimeDirectoryName"`
	SocketPath           string               `json:"socketPath"`
	Previous             *lifecycleRelease    `json:"previous,omitempty"`
	Pending              *lifecycleRelease    `json:"pending,omitempty"`
	Transition           *lifecycleTransition `json:"transition,omitempty"`
	PurgePaths           []string             `json:"purgePaths,omitempty"`
}

type lifecyclePurgeManifest struct {
	SchemaVersion int      `json:"schemaVersion"`
	ServiceName   string   `json:"serviceName"`
	Paths         []string `json:"paths"`
}

type lifecycleRelease struct {
	Executable       string   `json:"executable"`
	ExecutableSHA256 string   `json:"executableSHA256"`
	Version          string   `json:"version"`
	Arguments        []string `json:"arguments"`
}

type lifecycleTransition struct {
	Phase            string           `json:"phase"`
	Previous         lifecycleRelease `json:"previous"`
	Candidate        lifecycleRelease `json:"candidate"`
	SourcePID        int              `json:"sourcePID"`
	RecoveryRequired bool             `json:"recoveryRequired,omitempty"`
}

const (
	upgradeStoppingPrevious = "stopping-previous"
	upgradePreviousStopped  = "previous-stopped"
	upgradeSwitchingUnit    = "switching-unit"
	upgradeCandidateUnit    = "candidate-unit"
	upgradeCandidateStarted = "candidate-started"
)

func NewLifecycle(config LifecycleConfig, runner command.Runner) (*Lifecycle, error) {
	if runner == nil {
		return nil, errors.New("servicehost: lifecycle runner is required")
	}
	if config.CommandTimeout == 0 {
		config.CommandTimeout = DefaultLifecycleTimeout
	}
	if config.ReadinessTimeout == 0 {
		config.ReadinessTimeout = DefaultRPCTimeout
	}
	if config.DrainTimeout == 0 {
		config.DrainTimeout = DefaultDrainTimeout
	}
	if err := validateLifecycleConfig(config); err != nil {
		return nil, err
	}
	config.Paths.PurgePaths = append([]string(nil), config.Paths.PurgePaths...)
	return &Lifecycle{config: config, runner: runner}, nil
}

func (l *Lifecycle) Install(ctx context.Context, request InstallRequest) (InstallResult, error) {
	if ctx == nil {
		panic("nil context")
	}
	if !request.Consent {
		return InstallResult{}, ErrConsentRequired
	}
	if err := ctx.Err(); err != nil {
		return InstallResult{}, err
	}
	desired, source, err := l.descriptorForInstall(request)
	if err != nil {
		return InstallResult{}, err
	}
	defer source.Close()
	result := InstallResult{
		UnitPath: l.config.Paths.UnitPath, DescriptorPath: l.config.Paths.DescriptorPath,
		Executable: desired.Executable, Version: desired.Version,
	}
	unit, err := renderLifecycleUnit(l.config, desired)
	if err != nil {
		return result, err
	}
	descriptorBytes, err := marshalLifecycleDescriptor(desired)
	if err != nil {
		return result, err
	}
	if !validLifecycleDescriptorSize(descriptorBytes) {
		return result, errors.New("servicehost: lifecycle descriptor exceeds size limit")
	}
	purgeManifest, err := l.purgeManifestBytes()
	if err != nil {
		return result, err
	}
	if err := ensureLifecycleDirectories(l.config.Paths); err != nil {
		return result, err
	}
	lock, err := acquireLifecycleLock(l.config.Paths.DescriptorPath+".lock", uint32(os.Getuid()))
	if err != nil {
		return result, err
	}
	defer lock.Close()

	existing, exists, err := l.readDescriptor()
	if err != nil {
		return result, err
	}
	if exists {
		if existing.Transition != nil {
			return result, ErrUpgradeRecoveryRequired
		}
		if desiredComparable(existing) != desiredComparable(desired) {
			return result, ErrUpgradeRequired
		}
		if err := verifyLifecycleFile(l.config.Paths.UnitPath, unit, 0o600); err != nil {
			return result, err
		}
		if err := verifyExecutable(desired.Executable, desired.ExecutableSHA256); err != nil {
			return result, err
		}
		if err := l.verifyPurgeManifest(purgeManifest); err != nil {
			return result, err
		}
		if err := l.daemonReload(ctx); err != nil {
			return result, err
		}
		return result, nil
	}
	retainedPurgeManifest := false
	if len(purgeManifest) != 0 {
		if _, err := os.Lstat(l.purgeManifestPath()); err == nil {
			if err := l.verifyPurgeManifest(purgeManifest); err != nil {
				return result, err
			}
			retainedPurgeManifest = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	installPaths := []string{l.config.Paths.UnitPath, l.config.Paths.DescriptorPath, desired.Executable}
	if !retainedPurgeManifest {
		installPaths = append(installPaths, l.purgeManifestPath())
	}
	for _, path := range installPaths {
		if _, err := os.Lstat(path); err == nil {
			return result, fmt.Errorf("%w: unowned path exists: %s", ErrLifecycleOwnership, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, fmt.Errorf("servicehost: inspect install path: %w", err)
		}
	}

	created := make([]ownedCreatedFile, 0, 4)
	rollback := func(cause error) error {
		for index := len(created) - 1; index >= 0; index-- {
			cause = errors.Join(cause, created[index].remove())
		}
		return cause
	}
	binary, err := copyNewExecutable(source, desired.Executable)
	if err != nil {
		return result, err
	}
	created = append(created, binary)
	if err := verifyExecutable(desired.Executable, desired.ExecutableSHA256); err != nil {
		return result, rollback(err)
	}
	unitFile, err := writeNewLifecycleFile(l.config.Paths.UnitPath, unit, 0o600)
	if err != nil {
		return result, rollback(err)
	}
	created = append(created, unitFile)
	descriptorFile, err := writeNewLifecycleFile(l.config.Paths.DescriptorPath, descriptorBytes, 0o600)
	if err != nil {
		return result, rollback(err)
	}
	created = append(created, descriptorFile)
	if len(purgeManifest) != 0 && !retainedPurgeManifest {
		manifestFile, err := writeNewLifecycleFile(l.purgeManifestPath(), purgeManifest, 0o600)
		if err != nil {
			return result, rollback(err)
		}
		created = append(created, manifestFile)
	}
	if err := l.daemonReload(ctx); err != nil {
		return result, rollback(err)
	}
	result.Changed = true
	return result, nil
}

func (l *Lifecycle) StageUpgrade(ctx context.Context, request InstallRequest) (UpgradeStageResult, error) {
	if ctx == nil {
		panic("nil context")
	}
	if !request.Consent {
		return UpgradeStageResult{}, ErrConsentRequired
	}
	if err := ctx.Err(); err != nil {
		return UpgradeStageResult{}, err
	}
	desired, source, err := l.descriptorForInstall(request)
	if err != nil {
		return UpgradeStageResult{}, err
	}
	defer source.Close()
	candidate := releaseFromDescriptor(desired)
	result := UpgradeStageResult{PendingVersion: candidate.Version, Executable: candidate.Executable}
	if _, err := l.verifyInstallation(); err != nil {
		return result, err
	}
	lock, err := l.lockInstalled()
	if err != nil {
		return result, err
	}
	defer lock.Close()
	descriptor, err := l.verifyInstallation()
	if err != nil {
		return result, err
	}
	result.CurrentVersion = descriptor.Version
	current := releaseFromDescriptor(descriptor)
	if sameLifecycleRelease(current, candidate) {
		result.PendingVersion = descriptor.Version
		result.Executable = descriptor.Executable
		return result, nil
	}
	if candidate.Version == current.Version {
		return result, ErrUpgradeRequired
	}
	if descriptor.Pending != nil {
		if sameLifecycleRelease(*descriptor.Pending, candidate) {
			return result, verifyExecutable(candidate.Executable, candidate.ExecutableSHA256)
		}
		return result, ErrUpgradeRequired
	}

	var created *ownedCreatedFile
	fail := func(cause error) (UpgradeStageResult, error) {
		if created != nil {
			cause = errors.Join(cause, created.remove())
		}
		return result, cause
	}
	if _, statErr := os.Lstat(candidate.Executable); errors.Is(statErr, os.ErrNotExist) {
		file, copyErr := copyNewExecutable(source, candidate.Executable)
		if copyErr != nil {
			return result, copyErr
		}
		created = &file
	} else if statErr != nil {
		return result, fmt.Errorf("servicehost: inspect staged executable: %w", statErr)
	} else {
		if err := validateLifecycleDirectory(filepath.Dir(candidate.Executable), true, uint32(os.Getuid())); err != nil {
			return result, err
		}
	}
	if err := validateLifecycleDirectory(filepath.Dir(candidate.Executable), true, uint32(os.Getuid())); err != nil {
		return fail(err)
	}
	if err := verifyExecutable(candidate.Executable, candidate.ExecutableSHA256); err != nil {
		return fail(err)
	}
	before, err := marshalLifecycleDescriptor(descriptor)
	if err != nil {
		return fail(err)
	}
	descriptor.Pending = cloneLifecycleRelease(candidate)
	if !l.validDescriptor(descriptor) {
		return fail(fmt.Errorf("%w: invalid staged descriptor", ErrLifecycleOwnership))
	}
	after, err := marshalLifecycleDescriptor(descriptor)
	if err != nil {
		return fail(err)
	}
	if !validLifecycleDescriptorSize(after) {
		return fail(errors.New("servicehost: lifecycle descriptor exceeds size limit"))
	}
	if err := replaceLifecycleFile(l.config.Paths.DescriptorPath, before, after, 0o600, true); err != nil {
		return fail(err)
	}
	result.Changed = true
	return result, nil
}

func (l *Lifecycle) ActivateUpgrade(ctx context.Context, request UpgradeActivationRequest) (UpgradeActivationResult, error) {
	if ctx == nil {
		panic("nil context")
	}
	if !request.Confirm {
		return UpgradeActivationResult{}, ErrConsentRequired
	}
	if request.RollbackCheck == nil {
		return UpgradeActivationResult{}, errors.New("servicehost: rollback compatibility check is required")
	}
	if err := ctx.Err(); err != nil {
		return UpgradeActivationResult{}, err
	}
	if _, err := l.verifyInstallation(); err != nil {
		return UpgradeActivationResult{}, err
	}
	lock, err := l.lockInstalled()
	if err != nil {
		return UpgradeActivationResult{}, err
	}
	defer lock.Close()
	descriptor, err := l.verifyInstallation()
	if err != nil {
		return UpgradeActivationResult{}, err
	}
	if descriptor.Pending == nil {
		return UpgradeActivationResult{}, ErrUpgradeRequired
	}
	previous := releaseFromDescriptor(descriptor)
	candidate := *cloneLifecycleRelease(*descriptor.Pending)
	result := UpgradeActivationResult{CurrentVersion: previous.Version, PreviousVersion: previous.Version}
	before, err := l.ownedManagerStatus(ctx, descriptor)
	if err != nil {
		return result, err
	}
	client, err := l.drainActive(ctx, descriptor, before.Active)
	if err != nil {
		return result, err
	}
	descriptor.Transition = &lifecycleTransition{
		Phase: upgradeStoppingPrevious, Previous: previous, Candidate: candidate, SourcePID: before.MainPID,
	}
	if err := l.publishDescriptorTransition(descriptor); err != nil {
		return result, l.resumeAfterFailedMutation(client, err)
	}
	if before.Active {
		if _, err := l.runSystemctl(ctx, "stop", descriptor.UnitName); err != nil {
			return l.restorePreviousBeforeReplacement(descriptor, client, before, result, err)
		}
	}
	stopped, err := l.managerStatus(ctx, descriptor)
	if err != nil {
		return l.restorePreviousBeforeReplacement(descriptor, client, before, result, err)
	}
	if !terminalStopped(stopped) {
		if stopped.Active && stopped.MainPID == before.MainPID {
			abortErr := l.abortUpgradeBeforeReplacement(descriptor, client, errors.New("servicehost: previous service remained active during upgrade"))
			result.RecoveryRequired = errors.Is(abortErr, ErrUpgradeRecoveryRequired)
			return result, abortErr
		}
		_ = closeLifecycleClient(client)
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, errors.New("servicehost: previous service stop was not terminal"))
	}
	_ = closeLifecycleClient(client)
	if err := l.setUpgradePhase(&descriptor, upgradePreviousStopped); err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, err)
	}
	if err := l.setUpgradePhase(&descriptor, upgradeSwitchingUnit); err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, err)
	}
	currentUnit, err := renderLifecycleUnit(l.config, descriptor)
	if err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, err)
	}
	candidateDescriptor := descriptorWithRelease(descriptor, candidate)
	candidateUnit, err := renderLifecycleUnit(l.config, candidateDescriptor)
	if err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, err)
	}
	if err := replaceLifecycleFile(l.config.Paths.UnitPath, currentUnit, candidateUnit, 0o600, false); err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, err)
	}
	if err := l.setUpgradePhase(&descriptor, upgradeCandidateUnit); err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, err)
	}
	if err := l.daemonReload(ctx); err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, err)
	}
	if _, err := l.runSystemctl(ctx, "start", descriptor.UnitName); err != nil {
		return l.rollbackFailedCandidate(ctx, descriptor, candidateDescriptor, request.RollbackCheck, result, err)
	}
	started, err := l.managerStatus(ctx, candidateDescriptor)
	if err != nil {
		return l.rollbackFailedCandidate(ctx, descriptor, candidateDescriptor, request.RollbackCheck, result, err)
	}
	if !activeProcess(started) || before.Active && started.MainPID == before.MainPID {
		return l.rollbackFailedCandidate(ctx, descriptor, candidateDescriptor, request.RollbackCheck, result, fmt.Errorf("servicehost: candidate process did not start with a new identity: before=%d active=%t after=%d", before.MainPID, started.Active, started.MainPID))
	}
	if err := l.setUpgradePhase(&descriptor, upgradeCandidateStarted); err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, l.config.ReadinessTimeout)
	err = l.awaitReady(readyCtx, candidateDescriptor)
	cancel()
	if err != nil {
		return l.rollbackFailedCandidate(ctx, descriptor, candidateDescriptor, request.RollbackCheck, result, err)
	}
	ready, err := l.managerStatus(ctx, candidateDescriptor)
	if err != nil {
		return l.rollbackFailedCandidate(ctx, descriptor, candidateDescriptor, request.RollbackCheck, result, err)
	}
	if !activeProcess(ready) {
		err = errors.New("servicehost: candidate passed readiness but is not active")
		return l.rollbackFailedCandidate(ctx, descriptor, candidateDescriptor, request.RollbackCheck, result, err)
	}
	final := candidateDescriptor
	final.Previous = cloneLifecycleRelease(previous)
	final.Pending = nil
	final.Transition = nil
	if err := l.replaceDescriptor(descriptor, final); err != nil {
		return l.rollbackFailedCandidate(ctx, descriptor, candidateDescriptor, request.RollbackCheck, result, err)
	}
	result.Status = ready
	result.CurrentVersion = candidate.Version
	result.PreviousVersion = previous.Version
	return result, nil
}

func (l *Lifecycle) restorePreviousBeforeReplacement(
	descriptor lifecycleDescriptor,
	client *Client,
	before LifecycleStatus,
	result UpgradeActivationResult,
	cause error,
) (UpgradeActivationResult, error) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), l.previousRestoreTimeout())
	defer cleanupCancel()
	current, statusErr := l.managerStatus(cleanupCtx, descriptor)
	if sameProcess(current, before) {
		abortErr := l.abortUpgradeBeforeReplacement(descriptor, client, cause)
		result.Status = current
		result.RecoveryRequired = errors.Is(abortErr, ErrUpgradeRecoveryRequired)
		return result, abortErr
	}
	_ = closeLifecycleClient(client)
	statusErr = terminalStatusError(current, statusErr, "servicehost: interrupted previous stop was not terminal")
	if statusErr != nil {
		result.Status = current
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, statusErr))
	}
	if _, err := l.runSystemctl(cleanupCtx, "start", descriptor.UnitName); err != nil {
		return l.failPreviousRestore(cleanupCtx, descriptor, result, errors.Join(cause, err))
	}
	started, err := l.managerStatus(cleanupCtx, descriptor)
	if err != nil {
		return l.failPreviousRestore(cleanupCtx, descriptor, result, errors.Join(cause, err))
	}
	if !activeProcess(started) {
		err = errors.New("servicehost: previous process did not restart after interrupted upgrade")
		return l.failPreviousRestore(cleanupCtx, descriptor, result, errors.Join(cause, err))
	}
	readyCtx, readyCancel := context.WithTimeout(cleanupCtx, l.config.ReadinessTimeout)
	err = l.awaitReady(readyCtx, descriptor)
	readyCancel()
	if err != nil {
		return l.failPreviousRestore(cleanupCtx, descriptor, result, errors.Join(cause, err))
	}
	ready, err := l.managerStatus(cleanupCtx, descriptor)
	if err != nil {
		return l.failPreviousRestore(cleanupCtx, descriptor, result, errors.Join(cause, err))
	}
	if !activeProcess(ready) {
		err = errors.New("servicehost: restored previous process is not active")
		return l.failPreviousRestore(cleanupCtx, descriptor, result, errors.Join(cause, err))
	}
	cleared := descriptor
	cleared.Transition = nil
	if err := l.replaceDescriptor(descriptor, cleared); err != nil {
		return l.failPreviousRestore(cleanupCtx, descriptor, result, errors.Join(cause, err))
	}
	result.Status = ready
	return result, cause
}

func (l *Lifecycle) failPreviousRestore(_ context.Context, descriptor lifecycleDescriptor, result UpgradeActivationResult, cause error) (UpgradeActivationResult, error) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), l.finalStopTimeout())
	defer cleanupCancel()
	_, stopErr := l.runSystemctl(cleanupCtx, "stop", descriptor.UnitName)
	stopped, statusErr := l.managerStatus(cleanupCtx, descriptor)
	statusErr = terminalStatusError(stopped, statusErr, "servicehost: failed previous restore did not stop terminally")
	result.Status = stopped
	result.RecoveryRequired = true
	return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, stopErr, statusErr))
}

func (l *Lifecycle) rollbackFailedCandidate(
	_ context.Context,
	descriptor lifecycleDescriptor,
	candidateDescriptor lifecycleDescriptor,
	check RollbackCompatibilityCheck,
	result UpgradeActivationResult,
	cause error,
) (UpgradeActivationResult, error) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), l.rollbackCleanupTimeout())
	defer cleanupCancel()
	ctx := cleanupCtx
	if _, err := l.runSystemctl(ctx, "stop", descriptor.UnitName); err != nil {
		cause = errors.Join(cause, err)
	}
	stopped, stopErr := l.managerStatus(ctx, candidateDescriptor)
	stopErr = terminalStatusError(stopped, stopErr, "servicehost: candidate stop was not terminal")
	if stopErr != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, stopErr))
	}
	checkCtx, cancel := context.WithTimeout(ctx, l.config.CommandTimeout)
	compatible, checkErr := check(checkCtx, publicUpgradeRelease(descriptor.Transition.Previous), publicUpgradeRelease(descriptor.Transition.Candidate))
	cancel()
	if checkErr != nil {
		result.Status = stopped
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, checkErr))
	}
	if !compatible {
		checkErr = errors.New("servicehost: current state is not backward-readable")
		result.Status = stopped
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, checkErr))
	}
	if err := l.setUpgradePhase(&descriptor, upgradeSwitchingUnit); err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, err))
	}
	candidateUnit, err := renderLifecycleUnit(l.config, candidateDescriptor)
	if err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, err))
	}
	previousDescriptor := descriptorWithRelease(descriptor, descriptor.Transition.Previous)
	previousUnit, err := renderLifecycleUnit(l.config, previousDescriptor)
	if err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, err))
	}
	if err := replaceLifecycleFile(l.config.Paths.UnitPath, candidateUnit, previousUnit, 0o600, false); err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, err))
	}
	if err := l.setUpgradePhase(&descriptor, upgradePreviousStopped); err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, err))
	}
	if err := l.daemonReload(ctx); err != nil {
		result.RecoveryRequired = true
		return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, err))
	}
	if _, err := l.runSystemctl(ctx, "start", descriptor.UnitName); err != nil {
		result.RecoveryRequired = true
		return l.failStoppedRollback(ctx, descriptor, previousDescriptor, result, errors.Join(cause, err))
	}
	rollbackStatus, err := l.managerStatus(ctx, previousDescriptor)
	if err != nil {
		result.RecoveryRequired = true
		return l.failStoppedRollback(ctx, descriptor, previousDescriptor, result, errors.Join(cause, err))
	}
	if !activeProcess(rollbackStatus) {
		err = errors.New("servicehost: rollback process did not start")
		result.RecoveryRequired = true
		return l.failStoppedRollback(ctx, descriptor, previousDescriptor, result, errors.Join(cause, err))
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, l.config.ReadinessTimeout)
	err = l.awaitReady(readyCtx, previousDescriptor)
	readyCancel()
	if err != nil {
		result.RecoveryRequired = true
		return l.failStoppedRollback(ctx, descriptor, previousDescriptor, result, errors.Join(cause, err))
	}
	rollbackStatus, err = l.managerStatus(ctx, previousDescriptor)
	if err != nil {
		result.RecoveryRequired = true
		return l.failStoppedRollback(ctx, descriptor, previousDescriptor, result, errors.Join(cause, err))
	}
	if !activeProcess(rollbackStatus) {
		err = errors.New("servicehost: rollback passed readiness but is not active")
		result.RecoveryRequired = true
		return l.failStoppedRollback(ctx, descriptor, previousDescriptor, result, errors.Join(cause, err))
	}
	final := previousDescriptor
	final.Transition = nil
	if err := l.replaceDescriptor(descriptor, final); err != nil {
		result.RecoveryRequired = true
		return l.failStoppedRollback(ctx, descriptor, previousDescriptor, result, errors.Join(cause, err))
	}
	result.Status = rollbackStatus
	result.CurrentVersion = descriptor.Transition.Previous.Version
	result.PreviousVersion = descriptor.Transition.Candidate.Version
	result.RolledBack = true
	return result, errors.Join(ErrUpgradeRolledBack, cause)
}

func (l *Lifecycle) failStoppedRollback(_ context.Context, descriptor, previousDescriptor lifecycleDescriptor, result UpgradeActivationResult, cause error) (UpgradeActivationResult, error) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), l.finalStopTimeout())
	defer cleanupCancel()
	ctx := cleanupCtx
	_, stopCommandErr := l.runSystemctl(ctx, "stop", descriptor.UnitName)
	stopped, statusErr := l.managerStatus(ctx, previousDescriptor)
	statusErr = terminalStatusError(stopped, statusErr, "servicehost: failed rollback did not stop terminally")
	result.Status = stopped
	result.RecoveryRequired = true
	return result, l.markUpgradeRecovery(descriptor, errors.Join(cause, stopCommandErr, statusErr))
}

func (l *Lifecycle) publishDescriptorTransition(next lifecycleDescriptor) error {
	current := next
	current.Transition = nil
	return l.replaceDescriptor(current, next)
}

func (l *Lifecycle) setUpgradePhase(descriptor *lifecycleDescriptor, phase string) error {
	if descriptor.Transition == nil {
		return ErrUpgradeRecoveryRequired
	}
	next := *descriptor
	transition := *descriptor.Transition
	transition.Phase = phase
	next.Transition = &transition
	if err := l.replaceDescriptor(*descriptor, next); err != nil {
		return err
	}
	*descriptor = next
	return nil
}

func (l *Lifecycle) abortUpgradeBeforeReplacement(descriptor lifecycleDescriptor, client *Client, cause error) error {
	cleared := descriptor
	cleared.Transition = nil
	clearErr := l.replaceDescriptor(descriptor, cleared)
	cause = l.resumeAfterFailedMutation(client, errors.Join(cause, clearErr))
	if clearErr != nil {
		return errors.Join(ErrUpgradeRecoveryRequired, cause)
	}
	return cause
}

func (l *Lifecycle) markUpgradeRecovery(descriptor lifecycleDescriptor, cause error) error {
	if descriptor.Transition == nil {
		return errors.Join(ErrUpgradeRecoveryRequired, cause)
	}
	next := descriptor
	transition := *descriptor.Transition
	transition.RecoveryRequired = true
	next.Transition = &transition
	if err := l.replaceDescriptor(descriptor, next); err != nil {
		cause = errors.Join(cause, err)
	}
	return errors.Join(ErrUpgradeRecoveryRequired, cause)
}

func (l *Lifecycle) replaceDescriptor(current, next lifecycleDescriptor) error {
	if !l.validDescriptor(next) {
		return fmt.Errorf("%w: invalid lifecycle descriptor transition", ErrLifecycleOwnership)
	}
	before, err := marshalLifecycleDescriptor(current)
	if err != nil {
		return err
	}
	after, err := marshalLifecycleDescriptor(next)
	if err != nil {
		return err
	}
	if !validLifecycleDescriptorSize(after) {
		return errors.New("servicehost: lifecycle descriptor exceeds size limit")
	}
	return replaceLifecycleFile(l.config.Paths.DescriptorPath, before, after, 0o600, true)
}

func terminalStopped(status LifecycleStatus) bool {
	return !status.Active && status.MainPID == 0 && (status.ActiveState == "inactive" || status.ActiveState == "failed")
}

func activeProcess(status LifecycleStatus) bool {
	return status.Active && status.MainPID > 0
}

func sameProcess(current, previous LifecycleStatus) bool {
	return activeProcess(current) && current.MainPID == previous.MainPID
}

func terminalStatusError(status LifecycleStatus, cause error, message string) error {
	if cause != nil {
		return cause
	}
	if !terminalStopped(status) {
		return fmt.Errorf("%s: state=%s pid=%d", message, status.ActiveState, status.MainPID)
	}
	return nil
}

func managerAbsent(status LifecycleStatus) bool {
	return status.LoadState == "not-found" && !status.Active && status.MainPID == 0
}

func safeUnloadedManager(status LifecycleStatus) bool {
	if status.LoadState != "not-found" {
		return false
	}
	return terminalStopped(status)
}

func (l *Lifecycle) previousRestoreTimeout() time.Duration {
	return 4*l.config.CommandTimeout + l.config.ReadinessTimeout
}

func (l *Lifecycle) rollbackCleanupTimeout() time.Duration {
	if l.upgradeCleanupTimeout != nil {
		return l.upgradeCleanupTimeout()
	}
	return 5*l.config.CommandTimeout + l.config.ReadinessTimeout
}

func (l *Lifecycle) finalStopTimeout() time.Duration {
	return 2 * l.config.CommandTimeout
}

func (l *Lifecycle) Uninstall(ctx context.Context, request UninstallRequest) (UninstallResult, error) {
	if ctx == nil {
		panic("nil context")
	}
	result := UninstallResult{
		UnitPath: l.config.Paths.UnitPath, DescriptorPath: l.config.Paths.DescriptorPath,
		Retained: []string{"plugin settings, history, and recovery state", "lifecycle and socket lock files", "user manager and linger configuration", "unrelated services and user data"},
	}
	if !request.Confirm {
		return result, ErrConsentRequired
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	descriptor, exists, err := l.readDescriptor()
	if err != nil {
		return result, err
	}
	if !exists {
		if _, err := os.Lstat(l.config.Paths.UnitPath); err == nil {
			return result, fmt.Errorf("%w: unit exists without descriptor", ErrLifecycleOwnership)
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		result.AlreadyAbsent = true
		return result, nil
	}
	lock, err := l.lockInstalled()
	if err != nil {
		return result, err
	}
	defer lock.Close()
	descriptor, exists, err = l.readDescriptor()
	if err != nil {
		return result, err
	}
	if !exists {
		return result, fmt.Errorf("%w: descriptor disappeared during uninstall", ErrLifecycleOwnership)
	}
	unitExists, releases, err := l.verifyUninstallInventory(descriptor)
	if err != nil {
		return result, err
	}

	manager, err := l.managerStatus(ctx, descriptor)
	if err != nil {
		return result, err
	}
	if manager.LoadState == "loaded" {
		if !unitExists {
			if !terminalStopped(manager) {
				return result, errors.New("servicehost: loaded service must be terminally stopped before missing-unit cleanup")
			}
			if _, err := l.runSystemctl(ctx, "disable", descriptor.UnitName); err != nil {
				return result, err
			}
		} else {
			before := manager
			if before.FragmentPath != descriptor.UnitPath {
				return result, fmt.Errorf("%w: loaded unit path changed", ErrLifecycleOwnership)
			}
			runningDescriptor := descriptorForRunningTransition(descriptor, before)
			client, err := l.drainActive(ctx, runningDescriptor, before.Active)
			if err != nil {
				return result, err
			}
			if _, err := l.runSystemctl(ctx, "disable", descriptor.UnitName); err != nil {
				return result, l.resumeAfterFailedMutation(client, err)
			}
			if before.Active {
				if _, err := l.runSystemctl(ctx, "stop", descriptor.UnitName); err != nil {
					return result, l.resumeAfterFailedMutation(client, err)
				}
			}
			stopped, err := l.managerStatus(ctx, descriptor)
			if err != nil {
				return result, l.resumeAfterFailedMutation(client, err)
			}
			if !terminalStopped(stopped) {
				return result, l.resumeAfterFailedMutation(client, fmt.Errorf("servicehost: uninstall stop was not terminal: state=%s pid=%d", stopped.ActiveState, stopped.MainPID))
			}
			_ = closeLifecycleClient(client)
		}
	} else if !safeUnloadedManager(manager) {
		return result, fmt.Errorf("servicehost: unsafe manager state during uninstall: load=%s active=%s pid=%d", manager.LoadState, manager.ActiveState, manager.MainPID)
	}
	removedSocket, err := removeInactiveLifecycleSocket(descriptor.SocketPath, uint32(os.Getuid()))
	if err != nil {
		return result, err
	}
	result.RemovedSocket = removedSocket
	for _, release := range releases {
		removed, err := removeVerifiedExecutable(release)
		if err != nil {
			return result, err
		}
		if removed {
			result.RemovedExecutables = append(result.RemovedExecutables, release.Executable)
		}
	}
	if err := removeEmptyLifecycleBinaryDirectory(l.config.Paths.BinaryDirectory); err != nil {
		return result, err
	}
	if unitExists {
		unit, err := expectedUninstallUnit(l.config, descriptor)
		if err != nil {
			return result, err
		}
		removed, err := removeVerifiedLifecycleFile(l.config.Paths.UnitPath, unit, 0o600)
		if err != nil {
			return result, err
		}
		result.RemovedUnit = removed
	}
	if err := l.daemonReload(ctx); err != nil {
		return result, err
	}
	absent, err := l.managerStatus(ctx, descriptor)
	if err != nil {
		return result, err
	}
	if !managerAbsent(absent) {
		return result, errors.New("servicehost: user manager retained uninstalled service")
	}
	descriptorBytes, err := marshalLifecycleDescriptor(descriptor)
	if err != nil {
		return result, err
	}
	removed, err := removeVerifiedLifecycleFile(l.config.Paths.DescriptorPath, descriptorBytes, 0o600)
	if err != nil {
		return result, err
	}
	result.RemovedDescriptor = removed
	return result, nil
}

func (l *Lifecycle) Purge(ctx context.Context, request PurgeRequest) (PurgeResult, error) {
	if ctx == nil {
		panic("nil context")
	}
	if !request.Confirm {
		return PurgeResult{}, ErrConsentRequired
	}
	if !request.ConfirmRecoveryLoss {
		return PurgeResult{}, ErrPurgeRecoveryConsentRequired
	}
	if err := ctx.Err(); err != nil {
		return PurgeResult{}, err
	}
	expectedManifest, err := l.purgeManifestBytes()
	if err != nil {
		return PurgeResult{}, err
	}
	manifestPath := l.purgeManifestPath()
	manifestInfo, manifestErr := os.Lstat(manifestPath)
	if errors.Is(manifestErr, os.ErrNotExist) {
		for _, path := range l.config.Paths.PurgePaths {
			if _, err := os.Lstat(path); err == nil {
				return PurgeResult{}, fmt.Errorf("%w: purge data exists without manifest", ErrLifecycleOwnership)
			} else if !errors.Is(err, os.ErrNotExist) {
				return PurgeResult{}, err
			}
		}
		return PurgeResult{AlreadyAbsent: true}, nil
	}
	if manifestErr != nil {
		return PurgeResult{}, manifestErr
	}
	if len(expectedManifest) == 0 {
		return PurgeResult{}, fmt.Errorf("%w: unexpected purge manifest", ErrLifecycleOwnership)
	}
	manifestFile, err := inspectCreatedFile(manifestPath)
	if err != nil {
		return PurgeResult{}, fmt.Errorf("%w: inspect purge manifest: %v", ErrLifecycleOwnership, err)
	}
	if !manifestFile.matches(manifestInfo) {
		return PurgeResult{}, fmt.Errorf("%w: purge manifest changed", ErrLifecycleOwnership)
	}
	if err := verifyLifecycleFile(manifestPath, expectedManifest, 0o600); err != nil {
		return PurgeResult{}, err
	}
	lock, err := l.lockInstalled()
	if err != nil {
		return PurgeResult{}, err
	}
	defer lock.Close()
	if _, exists, err := l.readDescriptor(); err != nil {
		return PurgeResult{}, err
	} else if exists {
		return PurgeResult{}, errors.New("servicehost: uninstall service before purge")
	}
	for _, path := range []string{l.config.Paths.UnitPath, l.config.Paths.BinaryDirectory, l.config.Paths.SocketPath} {
		if _, err := os.Lstat(path); err == nil {
			return PurgeResult{}, fmt.Errorf("%w: service resource remains before purge: %s", ErrLifecycleOwnership, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return PurgeResult{}, err
		}
	}
	manifestInfo, err = os.Lstat(manifestPath)
	if err != nil {
		return PurgeResult{}, fmt.Errorf("%w: recheck purge manifest: %v", ErrLifecycleOwnership, err)
	}
	if !manifestFile.matches(manifestInfo) {
		return PurgeResult{}, fmt.Errorf("%w: purge manifest changed after lock", ErrLifecycleOwnership)
	}
	if err := verifyLifecycleFile(manifestPath, expectedManifest, 0o600); err != nil {
		return PurgeResult{}, err
	}

	type purgeGroup struct {
		path    string
		entries []lifecyclePurgeEntry
	}
	var groups []purgeGroup
	entryCount := 0
	result := PurgeResult{}
	for _, path := range l.config.Paths.PurgePaths {
		entries, exists, err := collectLifecyclePurgeEntries(path, uint32(os.Getuid()), remainingPurgeEntries(entryCount))
		if err != nil {
			return result, err
		}
		if exists {
			entryCount += len(entries)
			groups = append(groups, purgeGroup{path: path, entries: entries})
		}
	}
	for _, group := range groups {
		for _, snapshot := range group.entries {
			if err := snapshot.remove(uint32(os.Getuid())); err != nil {
				return result, err
			}
		}
		result.RemovedPaths = append(result.RemovedPaths, group.path)
	}
	if err := removeCapturedLifecycleFile(manifestFile, manifestPath, expectedManifest, 0o600); err != nil {
		return result, err
	}
	result.RemovedManifest = true
	return result, nil
}

type lifecyclePurgeEntry struct {
	path string
	file ownedCreatedFile
	mode os.FileMode
}

func collectLifecyclePurgeEntries(path string, uid uint32, remaining int) ([]lifecyclePurgeEntry, bool, error) {
	if !validPurgeEntryBudget(remaining) {
		return nil, false, errors.New("servicehost: purge inventory exceeds entry limit")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := validateLifecyclePurgeAncestorChain(path); err != nil {
		return nil, false, err
	}
	file, err := inspectCreatedFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("%w: inspect purge path %s: %v", ErrLifecycleOwnership, path, err)
	}
	if !file.matches(info) {
		return nil, false, fmt.Errorf("%w: purge path changed %s", ErrLifecycleOwnership, path)
	}
	if !ownedByUID(info, uid) {
		return nil, false, fmt.Errorf("%w: purge path owner %s", ErrLifecycleOwnership, path)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("%w: invalid purge path %s", ErrLifecycleOwnership, path)
	}
	if info.IsDir() {
		if info.Mode().Perm() != 0o700 {
			return nil, false, fmt.Errorf("%w: purge directory metadata %s", ErrLifecycleOwnership, path)
		}
		children, err := os.ReadDir(path)
		if err != nil {
			return nil, false, err
		}
		remaining--
		var entries []lifecyclePurgeEntry
		for _, child := range children {
			childEntries, _, err := collectLifecyclePurgeEntries(filepath.Join(path, child.Name()), uid, remaining-len(entries))
			if err != nil {
				return nil, false, err
			}
			entries = append(entries, childEntries...)
		}
		entries = append(entries, lifecyclePurgeEntry{path: path, file: file, mode: info.Mode()})
		return entries, true, nil
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: purge file metadata %s", ErrLifecycleOwnership, path)
	}
	if info.Mode().Perm() != 0o600 {
		return nil, false, fmt.Errorf("%w: purge file metadata %s", ErrLifecycleOwnership, path)
	}
	return []lifecyclePurgeEntry{{path: path, file: file, mode: info.Mode()}}, true, nil
}

func validPurgeEntryBudget(remaining int) bool {
	return remaining >= 1
}

func remainingPurgeEntries(used int) int {
	return maxLifecyclePurgeEntries - used
}

func (entry lifecyclePurgeEntry) remove(uid uint32) error {
	if err := validateLifecyclePurgeAncestorChain(entry.path); err != nil {
		return fmt.Errorf("%w: recheck purge entry ancestors %s: %v", ErrLifecycleOwnership, entry.path, err)
	}
	info, err := os.Lstat(entry.path)
	if err != nil {
		return fmt.Errorf("%w: recheck purge entry %s: %v", ErrLifecycleOwnership, entry.path, err)
	}
	if !entry.file.matches(info) {
		return fmt.Errorf("%w: purge entry changed before removal: %s", ErrLifecycleOwnership, entry.path)
	}
	if !ownedByUID(info, uid) {
		return fmt.Errorf("%w: purge entry changed before removal: %s", ErrLifecycleOwnership, entry.path)
	}
	if info.Mode() != entry.mode {
		return fmt.Errorf("%w: purge entry changed before removal: %s", ErrLifecycleOwnership, entry.path)
	}
	return entry.file.remove()
}

func validateLifecyclePurgeAncestorChain(path string) error {
	if !cleanAbsolute(path) {
		return fmt.Errorf("%w: invalid purge path", ErrLifecycleOwnership)
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: purge path traverses symlink: %s", ErrLifecycleOwnership, current)
		}
	}
	return nil
}

func (l *Lifecycle) descriptorForInstall(request InstallRequest) (lifecycleDescriptor, *os.File, error) {
	if !cleanAbsolute(request.SourceExecutable) {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: valid absolute executable and version are required")
	}
	if !validLifecycleToken(request.Version) {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: valid absolute executable and version are required")
	}
	arguments := append([]string(nil), request.Arguments...)
	for _, argument := range arguments {
		if _, err := quoteUnitValue(argument); err != nil {
			return lifecycleDescriptor{}, nil, fmt.Errorf("servicehost: invalid service argument: %w", err)
		}
	}
	pathInfo, err := os.Lstat(request.SourceExecutable)
	if err != nil {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: executable path must name a regular non-symlink file")
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: executable path must name a regular non-symlink file")
	}
	if !pathInfo.Mode().IsRegular() {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: executable path must name a regular non-symlink file")
	}
	source, err := os.Open(request.SourceExecutable)
	if err != nil {
		return lifecycleDescriptor{}, nil, fmt.Errorf("servicehost: open executable: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			_ = source.Close()
		}
	}()
	info, err := source.Stat()
	if err != nil {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: executable must be a regular executable owned by the current user")
	}
	if !info.Mode().IsRegular() {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: executable must be a regular executable owned by the current user")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: executable must be a regular executable owned by the current user")
	}
	if !ownedByUID(info, uint32(os.Getuid())) {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: executable must be a regular executable owned by the current user")
	}
	if !sameFileIdentity(pathInfo, info) {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: executable must be a regular executable owned by the current user")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, source); err != nil {
		return lifecycleDescriptor{}, nil, fmt.Errorf("servicehost: hash executable: %w", err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return lifecycleDescriptor{}, nil, fmt.Errorf("servicehost: rewind executable: %w", err)
	}
	executable := filepath.Join(l.config.Paths.BinaryDirectory, request.Version, "service")
	versionDirectory := filepath.Dir(executable)
	if filepath.Dir(versionDirectory) != l.config.Paths.BinaryDirectory {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: version escapes the binary directory")
	}
	if filepath.Base(versionDirectory) != request.Version {
		return lifecycleDescriptor{}, nil, errors.New("servicehost: version escapes the binary directory")
	}
	failed = false
	return lifecycleDescriptor{
		SchemaVersion: lifecycleDescriptorVersion, UnitName: l.config.UnitName, UnitPath: l.config.Paths.UnitPath,
		ServiceName: l.config.ServiceName,
		Executable:  executable, ExecutableSHA256: hex.EncodeToString(digest.Sum(nil)), Version: request.Version,
		Arguments: arguments, ProtocolVersion: l.config.ProtocolVersion, RuntimeDirectory: l.config.Paths.RuntimeDirectory,
		RuntimeDirectoryName: l.config.Paths.RuntimeDirectoryName, SocketPath: l.config.Paths.SocketPath,
		PurgePaths: append([]string(nil), l.config.Paths.PurgePaths...),
	}, source, nil
}

func (l *Lifecycle) readDescriptor() (lifecycleDescriptor, bool, error) {
	info, statErr := os.Lstat(l.config.Paths.DescriptorPath)
	if errors.Is(statErr, os.ErrNotExist) {
		return lifecycleDescriptor{}, false, nil
	}
	if statErr != nil {
		return lifecycleDescriptor{}, false, fmt.Errorf("servicehost: inspect lifecycle descriptor: %w", statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return lifecycleDescriptor{}, false, fmt.Errorf("%w: descriptor metadata", ErrLifecycleOwnership)
	}
	if !info.Mode().IsRegular() {
		return lifecycleDescriptor{}, false, fmt.Errorf("%w: descriptor metadata", ErrLifecycleOwnership)
	}
	if info.Mode().Perm() != 0o600 {
		return lifecycleDescriptor{}, false, fmt.Errorf("%w: descriptor metadata", ErrLifecycleOwnership)
	}
	if !ownedByUID(info, uint32(os.Getuid())) {
		return lifecycleDescriptor{}, false, fmt.Errorf("%w: descriptor metadata", ErrLifecycleOwnership)
	}
	raw, err := os.ReadFile(l.config.Paths.DescriptorPath)
	if err != nil {
		return lifecycleDescriptor{}, false, fmt.Errorf("servicehost: read lifecycle descriptor: %w", err)
	}
	if !validLifecycleDescriptorSize(raw) {
		return lifecycleDescriptor{}, false, fmt.Errorf("%w: oversized descriptor", ErrLifecycleOwnership)
	}
	var descriptor lifecycleDescriptor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return lifecycleDescriptor{}, false, fmt.Errorf("%w: invalid descriptor", ErrLifecycleOwnership)
	}
	canonical, err := marshalLifecycleDescriptor(descriptor)
	if err != nil {
		return lifecycleDescriptor{}, false, fmt.Errorf("%w: descriptor changed", ErrLifecycleOwnership)
	}
	if !bytes.Equal(raw, canonical) || !l.validDescriptor(descriptor) {
		return lifecycleDescriptor{}, false, fmt.Errorf("%w: descriptor changed", ErrLifecycleOwnership)
	}
	return descriptor, true, nil
}

func (l *Lifecycle) Status(ctx context.Context) (LifecycleStatus, error) {
	if ctx == nil {
		panic("nil context")
	}
	descriptor, err := l.verifyInstallation()
	if errors.Is(err, ErrNotInstalled) {
		return LifecycleStatus{}, nil
	}
	if err != nil {
		return LifecycleStatus{}, err
	}
	return l.managerStatus(ctx, descriptor)
}

func (l *Lifecycle) Enable(ctx context.Context) (LifecycleStatus, error) {
	return l.setEnabled(ctx, true)
}

func (l *Lifecycle) Disable(ctx context.Context) (LifecycleStatus, error) {
	return l.setEnabled(ctx, false)
}

func (l *Lifecycle) Start(ctx context.Context) (LifecycleStatus, error) {
	if ctx == nil {
		panic("nil context")
	}
	if _, err := l.verifyInstallation(); err != nil {
		return LifecycleStatus{}, err
	}
	lock, err := l.lockInstalled()
	if err != nil {
		return LifecycleStatus{}, err
	}
	defer lock.Close()
	descriptor, err := l.verifyInstallation()
	if err != nil {
		return LifecycleStatus{}, err
	}
	if _, err := l.ownedManagerStatus(ctx, descriptor); err != nil {
		return LifecycleStatus{}, err
	}
	if _, err := l.runSystemctl(ctx, "start", descriptor.UnitName); err != nil {
		return LifecycleStatus{}, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, l.config.ReadinessTimeout)
	defer cancel()
	if err := l.awaitReady(readyCtx, descriptor); err != nil {
		return LifecycleStatus{}, err
	}
	result, err := l.managerStatus(ctx, descriptor)
	if err != nil {
		return LifecycleStatus{}, err
	}
	if !activeProcess(result) {
		return result, errors.New("servicehost: service passed readiness but is not active")
	}
	return result, nil
}

func (l *Lifecycle) Restart(ctx context.Context) (LifecycleStatus, error) {
	if ctx == nil {
		panic("nil context")
	}
	if _, err := l.verifyInstallation(); err != nil {
		return LifecycleStatus{}, err
	}
	lock, err := l.lockInstalled()
	if err != nil {
		return LifecycleStatus{}, err
	}
	defer lock.Close()
	descriptor, err := l.verifyInstallation()
	if err != nil {
		return LifecycleStatus{}, err
	}
	before, err := l.ownedManagerStatus(ctx, descriptor)
	if err != nil {
		return LifecycleStatus{}, err
	}
	client, err := l.drainActive(ctx, descriptor, before.Active)
	if err != nil {
		return LifecycleStatus{}, err
	}
	if _, err := l.runSystemctl(ctx, "restart", descriptor.UnitName); err != nil {
		return LifecycleStatus{}, l.resumeAfterFailedMutation(client, err)
	}
	after, err := l.managerStatus(ctx, descriptor)
	if err != nil {
		_ = closeLifecycleClient(client)
		return LifecycleStatus{}, err
	}
	if before.Active && sameProcess(after, before) {
		return after, l.resumeAfterFailedMutation(client, errors.New("servicehost: service process did not change after restart"))
	}
	_ = closeLifecycleClient(client)
	if !activeProcess(after) {
		return after, errors.New("servicehost: service is not active after restart")
	}
	readyCtx, cancel := context.WithTimeout(ctx, l.config.ReadinessTimeout)
	defer cancel()
	if err := l.awaitReady(readyCtx, descriptor); err != nil {
		return after, err
	}
	result, err := l.managerStatus(ctx, descriptor)
	if err != nil {
		return LifecycleStatus{}, err
	}
	if !activeProcess(result) {
		return result, errors.New("servicehost: service passed readiness but is not active")
	}
	return result, nil
}

func (l *Lifecycle) Stop(ctx context.Context) (LifecycleStatus, error) {
	if ctx == nil {
		panic("nil context")
	}
	if _, err := l.verifyInstallation(); err != nil {
		return LifecycleStatus{}, err
	}
	lock, err := l.lockInstalled()
	if err != nil {
		return LifecycleStatus{}, err
	}
	defer lock.Close()
	descriptor, err := l.verifyInstallation()
	if err != nil {
		return LifecycleStatus{}, err
	}
	before, err := l.managerStatus(ctx, descriptor)
	if err != nil {
		return LifecycleStatus{}, err
	}
	client, err := l.drainActive(ctx, descriptor, before.Active)
	if err != nil {
		return LifecycleStatus{}, err
	}
	if _, err := l.runSystemctl(ctx, "stop", descriptor.UnitName); err != nil {
		return LifecycleStatus{}, l.resumeAfterFailedMutation(client, err)
	}
	result, err := l.managerStatus(ctx, descriptor)
	if err != nil {
		return LifecycleStatus{}, l.resumeAfterFailedMutation(client, err)
	}
	if !terminalStopped(result) {
		return result, l.resumeAfterFailedMutation(client, fmt.Errorf("servicehost: service stop was not terminal: state=%s pid=%d", result.ActiveState, result.MainPID))
	}
	_ = closeLifecycleClient(client)
	return result, nil
}

func (l *Lifecycle) drainActive(ctx context.Context, descriptor lifecycleDescriptor, active bool) (*Client, error) {
	if !active {
		return nil, nil
	}
	drainCtx, cancel := context.WithTimeout(ctx, l.config.DrainTimeout)
	defer cancel()
	client, err := l.dialLifecycleClient(drainCtx, descriptor, minDuration(lifecycleDrainDialTimeout, l.config.DrainTimeout))
	if err == nil {
		_, err = client.Drain(drainCtx)
		if err == nil && l.afterDrainResponse != nil {
			err = l.afterDrainResponse()
		}
	}
	if err != nil {
		return nil, l.resumeAfterFailedMutation(client, err)
	}
	return client, nil
}

func (l *Lifecycle) resumeAfterFailedMutation(client *Client, cause error) error {
	if client == nil {
		return cause
	}
	resumeCtx, cancel := context.WithTimeout(context.Background(), DefaultResumeTimeout)
	_, resumeErr := client.Resume(resumeCtx)
	cancel()
	_ = client.Close()
	return errors.Join(cause, resumeErr)
}

func closeLifecycleClient(client *Client) error {
	if client == nil {
		return nil
	}
	return client.Close()
}

func (l *Lifecycle) awaitReady(ctx context.Context, descriptor lifecycleDescriptor) error {
	var lastErr error
	for {
		client, err := l.dialLifecycleClient(ctx, descriptor, minDuration(lifecycleReadyDialTimeout, l.config.ReadinessTimeout))
		if err == nil {
			health, healthErr := client.Health(ctx)
			closeErr := client.Close()
			if healthErr == nil {
				handshake := client.Handshake()
				if freshReadyStatus(health, handshake) {
					return closeErr
				}
				lastErr = errors.New("servicehost: service is not ready and accepting")
			} else {
				lastErr = healthErr
				if terminalReadinessError(healthErr) {
					return healthErr
				}
			}
		} else {
			lastErr = err
			if terminalReadinessError(err) {
				return err
			}
		}
		timer := time.NewTimer(lifecycleReadyPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(ErrLifecycleUnavailable, ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
}

func freshReadyStatus(health Status, handshake Handshake) bool {
	return health.State == HealthReady && health.Accepting && health.Epoch == handshake.Cursor.Epoch && health.Revision >= handshake.Cursor.Revision
}

func terminalReadinessError(err error) bool {
	return errors.Is(err, ErrLifecycleOwnership) || grpcstatus.Code(err) == codes.FailedPrecondition
}

func (l *Lifecycle) dialLifecycleClient(ctx context.Context, descriptor lifecycleDescriptor, timeout time.Duration) (*Client, error) {
	client, err := Dial(ctx, ClientConfig{
		SocketPath: descriptor.SocketPath, ProtocolVersion: descriptor.ProtocolVersion,
		ClientName: l.config.ClientName, DialTimeout: timeout, RPCTimeout: l.config.CommandTimeout,
		DrainTimeout: l.config.DrainTimeout,
	})
	if err != nil {
		return nil, err
	}
	handshake := client.Handshake()
	if handshake.ServiceName != descriptor.ServiceName || handshake.ServiceVersion != descriptor.Version {
		_ = client.Close()
		return nil, fmt.Errorf("%w: service handshake identity changed", ErrLifecycleOwnership)
	}
	return client, nil
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func (l *Lifecycle) setEnabled(ctx context.Context, enabled bool) (LifecycleStatus, error) {
	if ctx == nil {
		panic("nil context")
	}
	if _, err := l.verifyInstallation(); err != nil {
		return LifecycleStatus{}, err
	}
	lock, err := l.lockInstalled()
	if err != nil {
		return LifecycleStatus{}, err
	}
	defer lock.Close()
	descriptor, err := l.verifyInstallation()
	if err != nil {
		return LifecycleStatus{}, err
	}
	if _, err := l.ownedManagerStatus(ctx, descriptor); err != nil {
		return LifecycleStatus{}, err
	}
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	if _, err := l.runSystemctl(ctx, verb, descriptor.UnitName); err != nil {
		return LifecycleStatus{}, err
	}
	result, err := l.managerStatus(ctx, descriptor)
	if err != nil {
		return LifecycleStatus{}, err
	}
	if result.Enabled != enabled {
		return result, fmt.Errorf("servicehost: systemd did not %s service", verb)
	}
	return result, nil
}

func (l *Lifecycle) verifyInstallation() (lifecycleDescriptor, error) {
	descriptor, exists, err := l.readDescriptor()
	if err != nil {
		return lifecycleDescriptor{}, err
	}
	if !exists {
		if _, err := os.Lstat(l.config.Paths.UnitPath); err == nil {
			return lifecycleDescriptor{}, fmt.Errorf("%w: unit exists without descriptor", ErrLifecycleOwnership)
		} else if !errors.Is(err, os.ErrNotExist) {
			return lifecycleDescriptor{}, err
		}
		return lifecycleDescriptor{}, ErrNotInstalled
	}
	directoryChecks := []struct {
		path    string
		private bool
	}{
		{filepath.Dir(l.config.Paths.DescriptorPath), true},
		{l.config.Paths.BinaryDirectory, true},
		{filepath.Dir(descriptor.Executable), true},
		{filepath.Dir(l.config.Paths.UnitPath), false},
	}
	for _, release := range []*lifecycleRelease{descriptor.Previous, descriptor.Pending} {
		if release != nil {
			directoryChecks = append(directoryChecks, struct {
				path    string
				private bool
			}{filepath.Dir(release.Executable), true})
		}
	}
	for _, check := range directoryChecks {
		if err := validateLifecycleDirectory(check.path, check.private, uint32(os.Getuid())); err != nil {
			return lifecycleDescriptor{}, err
		}
	}
	if err := l.verifyDescriptorUnit(descriptor); err != nil {
		return lifecycleDescriptor{}, err
	}
	if err := verifyExecutable(descriptor.Executable, descriptor.ExecutableSHA256); err != nil {
		return lifecycleDescriptor{}, err
	}
	for _, release := range []*lifecycleRelease{descriptor.Previous, descriptor.Pending} {
		if release != nil {
			if err := verifyExecutable(release.Executable, release.ExecutableSHA256); err != nil {
				return lifecycleDescriptor{}, err
			}
		}
	}
	purgeManifest, err := l.purgeManifestBytes()
	if err != nil {
		return lifecycleDescriptor{}, err
	}
	if err := l.verifyPurgeManifest(purgeManifest); err != nil {
		return lifecycleDescriptor{}, err
	}
	if descriptor.Transition != nil {
		return descriptor, ErrUpgradeRecoveryRequired
	}
	return descriptor, nil
}

func (l *Lifecycle) verifyUninstallInventory(descriptor lifecycleDescriptor) (bool, []lifecycleRelease, error) {
	purgeManifest, err := l.purgeManifestBytes()
	if err != nil {
		return false, nil, err
	}
	if err := l.verifyPurgeManifest(purgeManifest); err != nil {
		return false, nil, err
	}
	for _, check := range []struct {
		path    string
		private bool
	}{
		{filepath.Dir(l.config.Paths.DescriptorPath), true},
		{filepath.Dir(l.config.Paths.UnitPath), false},
	} {
		if err := validateLifecycleDirectory(check.path, check.private, uint32(os.Getuid())); err != nil {
			return false, nil, err
		}
	}
	unitExists := true
	if _, err := os.Lstat(l.config.Paths.UnitPath); errors.Is(err, os.ErrNotExist) {
		unitExists = false
	} else if err != nil {
		return false, nil, err
	} else if _, err := expectedUninstallUnit(l.config, descriptor); err != nil {
		return false, nil, err
	}
	releases := uniqueLifecycleReleases(descriptor)
	expectedVersions := make(map[string]lifecycleRelease, len(releases))
	for _, release := range releases {
		expectedVersions[release.Version] = release
	}
	entries, err := os.ReadDir(l.config.Paths.BinaryDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return unitExists, releases, nil
	}
	if err != nil {
		return false, nil, err
	}
	if err := validateLifecycleDirectory(l.config.Paths.BinaryDirectory, true, uint32(os.Getuid())); err != nil {
		return false, nil, err
	}
	for _, entry := range entries {
		release, ok := expectedVersions[entry.Name()]
		if !ok {
			return false, nil, fmt.Errorf("%w: untracked binary inventory %s", ErrLifecycleOwnership, entry.Name())
		}
		if !entry.IsDir() {
			return false, nil, fmt.Errorf("%w: untracked binary inventory %s", ErrLifecycleOwnership, entry.Name())
		}
		versionDirectory := filepath.Join(l.config.Paths.BinaryDirectory, entry.Name())
		if err := validateLifecycleDirectory(versionDirectory, true, uint32(os.Getuid())); err != nil {
			return false, nil, err
		}
		children, err := os.ReadDir(versionDirectory)
		if err != nil {
			return false, nil, err
		}
		for _, child := range children {
			if child.Name() != "service" {
				return false, nil, fmt.Errorf("%w: untracked version inventory %s", ErrLifecycleOwnership, child.Name())
			}
			if child.IsDir() {
				return false, nil, fmt.Errorf("%w: untracked version inventory %s", ErrLifecycleOwnership, child.Name())
			}
		}
		if _, err := os.Lstat(release.Executable); err == nil {
			if err := verifyExecutable(release.Executable, release.ExecutableSHA256); err != nil {
				return false, nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, nil, err
		}
	}
	return unitExists, releases, nil
}

func uniqueLifecycleReleases(descriptor lifecycleDescriptor) []lifecycleRelease {
	releases := []lifecycleRelease{releaseFromDescriptor(descriptor)}
	seen := map[string]bool{descriptor.Version: true}
	for _, release := range []*lifecycleRelease{descriptor.Previous, descriptor.Pending} {
		if release == nil || seen[release.Version] {
			continue
		}
		releases = append(releases, *cloneLifecycleRelease(*release))
		seen[release.Version] = true
	}
	return releases
}

func descriptorForRunningTransition(descriptor lifecycleDescriptor, status LifecycleStatus) lifecycleDescriptor {
	if runningReleaseIsCurrent(descriptor.Transition, status) {
		return descriptor
	}
	return descriptorWithRelease(descriptor, descriptor.Transition.Candidate)
}

func runningReleaseIsCurrent(transition *lifecycleTransition, status LifecycleStatus) bool {
	return transition == nil || status.MainPID == transition.SourcePID || transitionUsesCurrentUnit(transition)
}

func transitionUsesCurrentUnit(transition *lifecycleTransition) bool {
	return transition == nil || transition.Phase == upgradeStoppingPrevious || transition.Phase == upgradePreviousStopped
}

func expectedUninstallUnit(config LifecycleConfig, descriptor lifecycleDescriptor) ([]byte, error) {
	current, err := renderLifecycleUnit(config, descriptor)
	if err != nil {
		return nil, err
	}
	if err := verifyLifecycleFile(config.Paths.UnitPath, current, 0o600); err == nil {
		return current, nil
	}
	if transitionUsesCurrentUnit(descriptor.Transition) {
		return nil, fmt.Errorf("%w: unit does not match current release", ErrLifecycleOwnership)
	}
	candidateDescriptor := descriptorWithRelease(descriptor, descriptor.Transition.Candidate)
	candidate, err := renderLifecycleUnit(config, candidateDescriptor)
	if err != nil {
		return nil, err
	}
	if err := verifyLifecycleFile(config.Paths.UnitPath, candidate, 0o600); err != nil {
		return nil, err
	}
	return candidate, nil
}

func removeVerifiedExecutable(release lifecycleRelease) (bool, error) {
	versionDirectory := filepath.Dir(release.Executable)
	directory, directoryErr := inspectCreatedFile(versionDirectory)
	if errors.Is(directoryErr, os.ErrNotExist) {
		return false, nil
	}
	if directoryErr != nil {
		return false, directoryErr
	}
	file, err := inspectCreatedFile(release.Executable)
	if errors.Is(err, os.ErrNotExist) {
		entries, readErr := os.ReadDir(versionDirectory)
		if readErr != nil {
			return false, readErr
		}
		if len(entries) != 0 {
			return false, fmt.Errorf("%w: version directory is not empty", ErrLifecycleOwnership)
		}
		if err := directory.remove(); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := verifyExecutable(release.Executable, release.ExecutableSHA256); err != nil {
		return false, err
	}
	if err := file.remove(); err != nil {
		return false, err
	}
	if err := directory.remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, fmt.Errorf("%w: remove version directory: %v", ErrLifecycleOwnership, err)
	}
	return true, nil
}

func removeEmptyLifecycleBinaryDirectory(path string) error {
	directory, captureErr := inspectCreatedFile(path)
	if errors.Is(captureErr, os.ErrNotExist) {
		return nil
	}
	if captureErr != nil {
		return captureErr
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("%w: binary directory is not empty", ErrLifecycleOwnership)
	}
	return directory.remove()
}

func removeVerifiedLifecycleFile(path string, contents []byte, mode os.FileMode) (bool, error) {
	file, err := inspectCreatedFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, removeCapturedLifecycleFile(file, path, contents, mode)
}

func removeCapturedLifecycleFile(file ownedCreatedFile, path string, contents []byte, mode os.FileMode) error {
	if err := verifyLifecycleFile(path, contents, mode); err != nil {
		return err
	}
	return file.remove()
}

func removeInactiveLifecycleSocket(path string, uid uint32) (bool, error) {
	file, err := inspectCreatedFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !file.matches(info) {
		return false, fmt.Errorf("%w: socket changed before validation", ErrLifecycleOwnership)
	}
	if err := validateSocketOwner(info, uid); err != nil || info.Mode().Perm() != 0o600 {
		return false, errors.Join(ErrLifecycleOwnership, err)
	}
	lock, err := acquireSocketLock(path+".lock", uid)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	connection, dialErr := net.DialTimeout("unix", path, staleSocketProbeTimeout)
	if dialErr == nil {
		_ = connection.Close()
		return false, ErrSocketActive
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return false, fmt.Errorf("servicehost: socket is not safely inactive: %w", dialErr)
	}
	return true, file.remove()
}

func (l *Lifecycle) verifyDescriptorUnit(descriptor lifecycleDescriptor) error {
	current, err := renderLifecycleUnit(l.config, descriptor)
	if err != nil {
		return err
	}
	if transitionUsesCurrentUnit(descriptor.Transition) {
		return verifyLifecycleFile(l.config.Paths.UnitPath, current, 0o600)
	}
	candidateDescriptor := descriptorWithRelease(descriptor, descriptor.Transition.Candidate)
	candidate, err := renderLifecycleUnit(l.config, candidateDescriptor)
	if err != nil {
		return err
	}
	if descriptor.Transition.Phase == upgradeSwitchingUnit {
		if err := verifyLifecycleFile(l.config.Paths.UnitPath, current, 0o600); err == nil {
			return nil
		}
	}
	return verifyLifecycleFile(l.config.Paths.UnitPath, candidate, 0o600)
}

func (l *Lifecycle) lockInstalled() (*lifecycleLock, error) {
	path := l.config.Paths.DescriptorPath + ".lock"
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: lifecycle lock metadata", ErrLifecycleOwnership)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: lifecycle lock metadata", ErrLifecycleOwnership)
	}
	if !ownedRegularFile(info, uint32(os.Getuid())) {
		return nil, fmt.Errorf("%w: lifecycle lock metadata", ErrLifecycleOwnership)
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%w: lifecycle lock metadata", ErrLifecycleOwnership)
	}
	return acquireLifecycleLock(path, uint32(os.Getuid()))
}

func (l *Lifecycle) managerStatus(ctx context.Context, descriptor lifecycleDescriptor) (LifecycleStatus, error) {
	result, err := l.runSystemctl(ctx, "show", descriptor.UnitName,
		"--property=Id", "--property=LoadState", "--property=ActiveState", "--property=SubState",
		"--property=Result", "--property=MainPID", "--property=FragmentPath", "--property=UnitFileState", "--no-pager")
	if err != nil {
		return LifecycleStatus{}, err
	}
	return parseLifecycleStatus(result.Stdout, descriptor)
}

func (l *Lifecycle) ownedManagerStatus(ctx context.Context, descriptor lifecycleDescriptor) (LifecycleStatus, error) {
	result, err := l.managerStatus(ctx, descriptor)
	if err != nil {
		return LifecycleStatus{}, err
	}
	if result.LoadState != "loaded" {
		return LifecycleStatus{}, fmt.Errorf("%w: manager did not resolve installed unit", ErrLifecycleOwnership)
	}
	if result.FragmentPath != descriptor.UnitPath {
		return LifecycleStatus{}, fmt.Errorf("%w: manager did not resolve installed unit", ErrLifecycleOwnership)
	}
	return result, nil
}

func (l *Lifecycle) runSystemctl(ctx context.Context, arguments ...string) (command.Result, error) {
	bounded, cancel := context.WithTimeout(ctx, l.config.CommandTimeout)
	defer cancel()
	args := append([]string{"--user"}, arguments...)
	result, err := l.runner.Run(bounded, "systemctl", args)
	if err != nil {
		return result, errors.Join(ErrLifecycleUnavailable, err)
	}
	return result, nil
}

func parseLifecycleStatus(raw []byte, descriptor lifecycleDescriptor) (LifecycleStatus, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return LifecycleStatus{}, errors.New("servicehost: invalid systemd status output")
		}
		if key == "" {
			return LifecycleStatus{}, errors.New("servicehost: invalid systemd status output")
		}
		if _, duplicate := values[key]; duplicate {
			return LifecycleStatus{}, errors.New("servicehost: duplicate systemd status field")
		}
		values[key] = value
	}
	for _, key := range []string{"Id", "LoadState", "ActiveState", "SubState", "Result", "MainPID", "FragmentPath", "UnitFileState"} {
		if _, ok := values[key]; !ok {
			return LifecycleStatus{}, fmt.Errorf("servicehost: missing systemd status field %s", key)
		}
	}
	mainPID, err := strconv.Atoi(values["MainPID"])
	if err != nil || mainPID < 0 || values["Id"] != descriptor.UnitName {
		return LifecycleStatus{}, errors.New("servicehost: invalid systemd service identity")
	}
	if values["LoadState"] == "loaded" && values["FragmentPath"] != descriptor.UnitPath {
		return LifecycleStatus{}, fmt.Errorf("%w: loaded unit path changed", ErrLifecycleOwnership)
	}
	return LifecycleStatus{
		Installed: true, Enabled: values["UnitFileState"] == "enabled" || values["UnitFileState"] == "enabled-runtime",
		Active: values["ActiveState"] == "active", UnitName: values["Id"], LoadState: values["LoadState"],
		ActiveState: values["ActiveState"], SubState: values["SubState"], Result: values["Result"], MainPID: mainPID,
		FragmentPath: values["FragmentPath"], UnitFileState: values["UnitFileState"],
	}, nil
}

func (l *Lifecycle) validDescriptor(descriptor lifecycleDescriptor) bool {
	current := releaseFromDescriptor(descriptor)
	if descriptor.SchemaVersion != lifecycleDescriptorVersion || descriptor.UnitName != l.config.UnitName ||
		descriptor.ServiceName != l.config.ServiceName || descriptor.UnitPath != l.config.Paths.UnitPath ||
		descriptor.ProtocolVersion != l.config.ProtocolVersion || descriptor.RuntimeDirectory != l.config.Paths.RuntimeDirectory ||
		descriptor.RuntimeDirectoryName != l.config.Paths.RuntimeDirectoryName || descriptor.SocketPath != l.config.Paths.SocketPath ||
		!slices.Equal(descriptor.PurgePaths, l.config.Paths.PurgePaths) ||
		!l.validRelease(current) {
		return false
	}
	for _, release := range []*lifecycleRelease{descriptor.Previous, descriptor.Pending} {
		if release != nil && (!l.validRelease(*release) || release.Version == current.Version) {
			return false
		}
	}
	if descriptor.Previous != nil && descriptor.Pending != nil && descriptor.Previous.Version == descriptor.Pending.Version {
		return false
	}
	if descriptor.Transition == nil {
		return true
	}
	transition := descriptor.Transition
	if descriptor.Pending == nil || transition.SourcePID < 0 || !sameLifecycleRelease(transition.Previous, current) ||
		!sameLifecycleRelease(transition.Candidate, *descriptor.Pending) {
		return false
	}
	switch transition.Phase {
	case upgradeStoppingPrevious, upgradePreviousStopped, upgradeSwitchingUnit, upgradeCandidateUnit, upgradeCandidateStarted:
		return true
	default:
		return false
	}
}

func (l *Lifecycle) validRelease(release lifecycleRelease) bool {
	if !cleanAbsolute(release.Executable) {
		return false
	}
	if !validLifecycleToken(release.Version) {
		return false
	}
	if release.Executable != filepath.Join(l.config.Paths.BinaryDirectory, release.Version, "service") {
		return false
	}
	if len(release.ExecutableSHA256) != sha256.Size*2 {
		return false
	}
	for _, argument := range release.Arguments {
		if _, err := quoteUnitValue(argument); err != nil {
			return false
		}
	}
	return true
}

func releaseFromDescriptor(descriptor lifecycleDescriptor) lifecycleRelease {
	return lifecycleRelease{
		Executable: descriptor.Executable, ExecutableSHA256: descriptor.ExecutableSHA256,
		Version: descriptor.Version, Arguments: append([]string(nil), descriptor.Arguments...),
	}
}

func cloneLifecycleRelease(release lifecycleRelease) *lifecycleRelease {
	release.Arguments = append([]string(nil), release.Arguments...)
	return &release
}

func sameLifecycleRelease(left, right lifecycleRelease) bool {
	return left.Executable == right.Executable && left.ExecutableSHA256 == right.ExecutableSHA256 &&
		left.Version == right.Version && slices.Equal(left.Arguments, right.Arguments)
}

func descriptorWithRelease(descriptor lifecycleDescriptor, release lifecycleRelease) lifecycleDescriptor {
	descriptor.Executable = release.Executable
	descriptor.ExecutableSHA256 = release.ExecutableSHA256
	descriptor.Version = release.Version
	descriptor.Arguments = append([]string(nil), release.Arguments...)
	return descriptor
}

func publicUpgradeRelease(release lifecycleRelease) UpgradeRelease {
	return UpgradeRelease{
		Executable: release.Executable, ExecutableSHA256: release.ExecutableSHA256,
		Version: release.Version, Arguments: append([]string(nil), release.Arguments...),
	}
}

func (l *Lifecycle) daemonReload(ctx context.Context) error {
	if _, err := l.runSystemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("servicehost: reload user service manager: %w", err)
	}
	return nil
}

type comparableLifecycleDescriptor struct {
	SchemaVersion, ProtocolVersion                                         uint32
	UnitName, ServiceName, UnitPath, Executable, ExecutableSHA256, Version string
	Arguments                                                              string
	RuntimeDirectory, RuntimeDirectoryName, SocketPath                     string
	PurgePaths                                                             string
}

func desiredComparable(descriptor lifecycleDescriptor) comparableLifecycleDescriptor {
	return comparableLifecycleDescriptor{
		SchemaVersion: uint32(descriptor.SchemaVersion), ProtocolVersion: descriptor.ProtocolVersion,
		UnitName: descriptor.UnitName, ServiceName: descriptor.ServiceName, UnitPath: descriptor.UnitPath, Executable: descriptor.Executable,
		ExecutableSHA256: descriptor.ExecutableSHA256, Version: descriptor.Version,
		Arguments: strings.Join(descriptor.Arguments, "\x00"), RuntimeDirectory: descriptor.RuntimeDirectory,
		RuntimeDirectoryName: descriptor.RuntimeDirectoryName, SocketPath: descriptor.SocketPath,
		PurgePaths: strings.Join(descriptor.PurgePaths, "\x00"),
	}
}

func validateLifecycleConfig(config LifecycleConfig) error {
	paths := config.Paths
	if filepath.Base(config.UnitName) != config.UnitName || !strings.HasSuffix(config.UnitName, ".service") || !validIdentity(config.ServiceName) ||
		config.Description == "" || len(config.Description) > 256 || !utf8.ValidString(config.Description) || strings.ContainsAny(config.Description, "\r\n\x00") ||
		config.ProtocolVersion == 0 || !validIdentity(config.ClientName) || config.CommandTimeout < 1 || config.ReadinessTimeout < 1 || config.DrainTimeout < 1 ||
		!cleanAbsolute(paths.UnitPath) || filepath.Base(paths.UnitPath) != config.UnitName || !cleanAbsolute(paths.DescriptorPath) ||
		!cleanAbsolute(paths.BinaryDirectory) || !cleanAbsolute(paths.RuntimeDirectory) || !cleanAbsolute(paths.SocketPath) ||
		filepath.Dir(paths.SocketPath) != paths.RuntimeDirectory || !validRuntimeDirectoryName(paths.RuntimeDirectoryName) ||
		!strings.HasSuffix(paths.RuntimeDirectory, string(filepath.Separator)+filepath.FromSlash(paths.RuntimeDirectoryName)) ||
		paths.BinaryDirectory == string(filepath.Separator) || paths.RuntimeDirectory == string(filepath.Separator) ||
		filepath.Dir(paths.DescriptorPath) == string(filepath.Separator) || filepath.Dir(paths.UnitPath) == string(filepath.Separator) {
		return errors.New("servicehost: invalid lifecycle configuration")
	}
	if !validLifecyclePurgePaths(paths) {
		return errors.New("servicehost: invalid lifecycle purge paths")
	}
	return nil
}

func validLifecyclePurgePaths(paths LifecyclePaths) bool {
	if len(paths.PurgePaths) > 64 {
		return false
	}
	protected := []string{
		paths.UnitPath, paths.DescriptorPath, paths.DescriptorPath + ".lock", paths.DescriptorPath + ".purge",
		paths.BinaryDirectory, paths.RuntimeDirectory, paths.SocketPath, paths.SocketPath + ".lock",
	}
	for index, path := range paths.PurgePaths {
		if !cleanAbsolute(path) || path == string(filepath.Separator) {
			return false
		}
		for _, reserved := range protected {
			if lifecyclePathContains(path, reserved) || lifecyclePathContains(reserved, path) {
				return false
			}
		}
		for otherIndex, other := range paths.PurgePaths {
			if index != otherIndex && (lifecyclePathContains(path, other) || lifecyclePathContains(other, path)) {
				return false
			}
		}
	}
	return true
}

func lifecyclePathContains(parent, child string) bool {
	if parent == child {
		return true
	}
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validRuntimeDirectoryName(value string) bool {
	return value != "" && !filepath.IsAbs(value) && filepath.Clean(value) == value && value != "." && value != ".." && !strings.HasPrefix(value, ".."+string(filepath.Separator)) && !strings.ContainsAny(value, "%$\x00\r\n")
}

func validLifecycleToken(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > MaxIdentityBytes || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._+-", char)) {
			return false
		}
	}
	return true
}

func ensureLifecycleDirectories(paths LifecyclePaths) error {
	for _, path := range []string{filepath.Dir(paths.DescriptorPath), paths.BinaryDirectory} {
		if err := ensureLifecycleDirectory(path, true, uint32(os.Getuid())); err != nil {
			return err
		}
	}
	unitDirectory := filepath.Dir(paths.UnitPath)
	return ensureLifecycleDirectory(unitDirectory, false, uint32(os.Getuid()))
}

func ensureLifecycleDirectory(path string, private bool, uid uint32) error {
	if !cleanAbsolute(path) {
		return errors.New("servicehost: lifecycle directory must be a non-root clean absolute path")
	}
	if path == string(filepath.Separator) {
		return errors.New("servicehost: lifecycle directory must be a non-root clean absolute path")
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		created := false
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return fmt.Errorf("servicehost: create lifecycle directory: %w", err)
			}
			created = true
			info, err = os.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("%w: unsafe lifecycle directory: %s", ErrLifecycleOwnership, current)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: unsafe lifecycle directory: %s", ErrLifecycleOwnership, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: unsafe lifecycle directory: %s", ErrLifecycleOwnership, current)
		}
		if created {
			if err := os.Chmod(current, 0o700); err != nil {
				return fmt.Errorf("servicehost: protect lifecycle directory: %w", err)
			}
			info, err = os.Lstat(current)
			if err != nil {
				return fmt.Errorf("servicehost: inspect lifecycle directory: %w", err)
			}
		}
		if current == path {
			if !ownedByUID(info, uid) {
				return fmt.Errorf("%w: lifecycle directory metadata: %s", ErrLifecycleOwnership, path)
			}
			if private && info.Mode().Perm() != 0o700 {
				return fmt.Errorf("%w: lifecycle directory metadata: %s", ErrLifecycleOwnership, path)
			}
		}
	}
	return nil
}

func validateLifecycleDirectory(path string, private bool, uid uint32) error {
	if !cleanAbsolute(path) {
		return errors.New("servicehost: lifecycle directory must be a non-root clean absolute path")
	}
	if path == string(filepath.Separator) {
		return errors.New("servicehost: lifecycle directory must be a non-root clean absolute path")
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("%w: unsafe lifecycle directory: %s", ErrLifecycleOwnership, current)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: unsafe lifecycle directory: %s", ErrLifecycleOwnership, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: unsafe lifecycle directory: %s", ErrLifecycleOwnership, current)
		}
		if current == path {
			if !ownedByUID(info, uid) {
				return fmt.Errorf("%w: lifecycle directory metadata: %s", ErrLifecycleOwnership, path)
			}
			if private && info.Mode().Perm() != 0o700 {
				return fmt.Errorf("%w: lifecycle directory metadata: %s", ErrLifecycleOwnership, path)
			}
		}
	}
	return nil
}

func marshalLifecycleDescriptor(descriptor lifecycleDescriptor) ([]byte, error) {
	data, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func validLifecycleDescriptorSize(contents []byte) bool {
	return len(contents) <= maxLifecycleDescriptor
}

func (l *Lifecycle) purgeManifestPath() string {
	return l.config.Paths.DescriptorPath + ".purge"
}

func (l *Lifecycle) purgeManifestBytes() ([]byte, error) {
	if len(l.config.Paths.PurgePaths) == 0 {
		return nil, nil
	}
	manifest := lifecyclePurgeManifest{
		SchemaVersion: 1, ServiceName: l.config.ServiceName,
		Paths: append([]string(nil), l.config.Paths.PurgePaths...),
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (l *Lifecycle) verifyPurgeManifest(expected []byte) error {
	path := l.purgeManifestPath()
	if len(expected) == 0 {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("%w: unexpected purge manifest", ErrLifecycleOwnership)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return verifyLifecycleFile(path, expected, 0o600)
}

func renderLifecycleUnit(config LifecycleConfig, descriptor lifecycleDescriptor) ([]byte, error) {
	executable, err := quoteUnitValue(descriptor.Executable)
	if err != nil {
		return nil, err
	}
	arguments := []string{executable}
	for _, argument := range descriptor.Arguments {
		quoted, err := quoteUnitValue(argument)
		if err != nil {
			return nil, err
		}
		arguments = append(arguments, quoted)
	}
	runtimeDirectory, err := quoteUnitValue(descriptor.RuntimeDirectoryName)
	if err != nil {
		return nil, err
	}
	unit := "[Unit]\nDescription=" + config.Description + "\nStartLimitIntervalSec=60s\nStartLimitBurst=3\n\n" +
		"[Service]\nType=simple\nRuntimeDirectory=" + runtimeDirectory + "\nRuntimeDirectoryMode=0700\nUMask=0077\n" +
		"ExecStart=" + strings.Join(arguments, " ") + "\nRestart=on-failure\nRestartSec=2s\nTimeoutStopSec=70s\n\n" +
		"[Install]\nWantedBy=default.target\n"
	return []byte(unit), nil
}

func quoteUnitValue(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("empty value or control character")
	}
	var result strings.Builder
	result.WriteByte('"')
	for _, char := range value {
		switch char {
		case '\\':
			result.WriteString("\\\\")
		case '"':
			result.WriteString("\\\"")
		case '%':
			result.WriteString("%%")
		case '$':
			result.WriteString("$$")
		default:
			result.WriteRune(char)
		}
	}
	result.WriteByte('"')
	return result.String(), nil
}

type ownedCreatedFile struct {
	path string
	stat syscall.Stat_t
}

func writeNewLifecycleFile(path string, data []byte, mode os.FileMode) (ownedCreatedFile, error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".servicehost-*")
	if err != nil {
		return ownedCreatedFile{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return ownedCreatedFile{}, err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return ownedCreatedFile{}, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return ownedCreatedFile{}, err
	}
	if err := temporary.Close(); err != nil {
		return ownedCreatedFile{}, err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ownedCreatedFile{}, fmt.Errorf("%w: path appeared during install: %s", ErrLifecycleOwnership, path)
		}
		return ownedCreatedFile{}, err
	}
	return inspectCreatedFile(path)
}

func replaceLifecycleFile(path string, current, next []byte, mode os.FileMode, privateDirectory bool) error {
	if err := verifyLifecycleFile(path, current, mode); err != nil {
		return err
	}
	if err := validateLifecycleDirectory(filepath.Dir(path), privateDirectory, uint32(os.Getuid())); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".servicehost-replace-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(next); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := verifyLifecycleFile(path, current, mode); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func copyNewExecutable(source *os.File, path string) (ownedCreatedFile, error) {
	if err := ensureLifecycleDirectory(filepath.Dir(path), true, uint32(os.Getuid())); err != nil {
		return ownedCreatedFile{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".servicehost-bin-*")
	if err != nil {
		return ownedCreatedFile{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return ownedCreatedFile{}, err
	}
	if _, err := io.Copy(temporary, source); err != nil {
		_ = temporary.Close()
		return ownedCreatedFile{}, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return ownedCreatedFile{}, err
	}
	if err := temporary.Close(); err != nil {
		return ownedCreatedFile{}, err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ownedCreatedFile{}, fmt.Errorf("%w: executable appeared during install", ErrLifecycleOwnership)
		}
		return ownedCreatedFile{}, err
	}
	return inspectCreatedFile(path)
}

func inspectCreatedFile(path string) (ownedCreatedFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return ownedCreatedFile{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ownedCreatedFile{}, fmt.Errorf("servicehost: unexpected file metadata %T", info.Sys())
	}
	return ownedCreatedFile{path: path, stat: *stat}, nil
}

func (file ownedCreatedFile) remove() error {
	info, err := os.Lstat(file.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !sameStatIdentity(info, file.stat) {
		return fmt.Errorf("%w: created path changed before rollback: %s", ErrLifecycleOwnership, file.path)
	}
	return os.Remove(file.path)
}

func (file ownedCreatedFile) matches(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Dev == file.stat.Dev && stat.Ino == file.stat.Ino
}

func verifyLifecycleFile(path string, want []byte, mode os.FileMode) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", ErrLifecycleOwnership, path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: file changed: %s", ErrLifecycleOwnership, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: file changed: %s", ErrLifecycleOwnership, path)
	}
	if !ownedByUID(info, uint32(os.Getuid())) {
		return fmt.Errorf("%w: file changed: %s", ErrLifecycleOwnership, path)
	}
	if info.Mode().Perm() != mode || !bytes.Equal(got, want) {
		return fmt.Errorf("%w: file changed: %s", ErrLifecycleOwnership, path)
	}
	return nil
}

func verifyExecutable(path, wantDigest string) error {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: executable path", ErrLifecycleOwnership)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: executable path", ErrLifecycleOwnership)
	}
	if !pathInfo.Mode().IsRegular() {
		return fmt.Errorf("%w: executable path", ErrLifecycleOwnership)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("%w: executable path traverses a symlink", ErrLifecycleOwnership)
	}
	if resolved != path {
		return fmt.Errorf("%w: executable path traverses a symlink", ErrLifecycleOwnership)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: executable missing", ErrLifecycleOwnership)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: executable metadata", ErrLifecycleOwnership)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: executable metadata", ErrLifecycleOwnership)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: executable metadata", ErrLifecycleOwnership)
	}
	if !ownedByUID(info, uint32(os.Getuid())) {
		return fmt.Errorf("%w: executable metadata", ErrLifecycleOwnership)
	}
	if !sameFileIdentity(pathInfo, info) {
		return fmt.Errorf("%w: executable metadata", ErrLifecycleOwnership)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil || hex.EncodeToString(digest.Sum(nil)) != wantDigest {
		return fmt.Errorf("%w: executable contents", ErrLifecycleOwnership)
	}
	return nil
}

func ownedByUID(info os.FileInfo, uid uint32) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uid
}

func ownedRegularFile(info os.FileInfo, uid uint32) bool {
	return info.Mode().IsRegular() && ownedByUID(info, uid)
}

func sameFileIdentity(left, right os.FileInfo) bool {
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && leftStat.Dev == rightStat.Dev && leftStat.Ino == rightStat.Ino
}
