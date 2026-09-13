package sessionhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrapi"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrid"
)

const (
	MaxArgv                         = 128
	MaxArgBytes                     = 4096
	MaxCommandLen                   = 64 << 10
	ShellReadyTimeout time.Duration = 5_000_000_000 // 5 seconds
)

var (
	ErrStaleTarget      = errors.New("Herdr target is stale")
	ErrAmbiguousTarget  = errors.New("Herdr target is ambiguous")
	ErrUnsupportedShell = errors.New("unsupported interactive shell")
)

type Placement string

const (
	NewTab  Placement = "new_tab"
	NewPane Placement = "new_pane"
)

type Origin struct {
	SessionName string
	WorkspaceID string
	TabID       string
	PaneID      string
}

type LaunchRequest struct {
	Origin          Origin
	Argv            []string
	Directory       string
	Placement       Placement
	Direction       string
	TabLabel        string
	Focus           bool
	OMPStatusSocket string
}

type CreatedTarget struct {
	Origin
	CreatedTab bool
	DirectArgv bool
}

type apiCaller interface {
	Call(context.Context, string, string, string, any, any) error
}

type wirePane struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	CWD         string `json:"cwd"`
	Focused     bool   `json:"focused"`
}

// Launch creates one background target in the exact originating session.
// New tabs use Herdr's literal command-array API. Herdr 0.8.2 cannot attach an
// arbitrary argv array to an existing pane, so same-tab launches use a newly
// split shell and one strictly quoted, kit-generated exec command.
func (h Host) Launch(ctx context.Context, request LaunchRequest) (CreatedTarget, error) {
	if err := h.validateLaunch(request); err != nil {
		return CreatedTarget{}, fmt.Errorf("launch: %w", err)
	}
	if _, err := h.currentPane(ctx, request.Origin); err != nil {
		return CreatedTarget{}, fmt.Errorf("launch origin: %w", err)
	}
	var (
		created CreatedTarget
		err     error
	)
	switch request.Placement {
	case NewTab:
		created, err = h.launchTab(ctx, request)
	case NewPane:
		created, err = h.launchPane(ctx, request)
	}
	if err != nil {
		return CreatedTarget{}, err
	}
	if _, err := h.currentPane(ctx, request.Origin); err != nil {
		cleanupErr := h.cleanupAfterFailure(ctx, created)
		return CreatedTarget{}, errors.Join(fmt.Errorf("launch origin changed: %w", err), cleanupErr)
	}
	if request.Focus {
		if err := h.FocusPane(ctx, created.Origin); err != nil {
			cleanupErr := h.cleanupAfterFailure(ctx, created)
			return CreatedTarget{}, errors.Join(err, cleanupErr)
		}
	}
	return created, nil
}

func (h Host) launchTab(ctx context.Context, request LaunchRequest) (CreatedTarget, error) {
	socket, err := h.sessionSocket(ctx)
	if err != nil {
		return CreatedTarget{}, fmt.Errorf("launch tab: %w", err)
	}
	params := struct {
		WorkspaceID string            `json:"workspace_id"`
		TabLabel    string            `json:"tab_label,omitempty"`
		Focus       bool              `json:"focus"`
		Env         map[string]string `json:"env,omitempty"`
		Root        layoutPane        `json:"root"`
	}{WorkspaceID: request.Origin.WorkspaceID, TabLabel: request.TabLabel, Focus: false, Env: launchEnv(request), Root: layoutPane{"pane", request.Directory, request.Argv}}
	var result struct {
		Type   string `json:"type"`
		Layout struct {
			WorkspaceID   string `json:"workspace_id"`
			TabID         string `json:"tab_id"`
			FocusedPaneID string `json:"focused_pane_id"`
			Root          struct {
				Type    string   `json:"type"`
				PaneID  string   `json:"pane_id"`
				CWD     string   `json:"cwd"`
				Command []string `json:"command"`
			} `json:"root"`
		} `json:"layout"`
	}
	if err := h.caller().Call(ctx, socket, "plugin-kit:layout.apply", "layout.apply", params, &result); err != nil {
		return CreatedTarget{}, fmt.Errorf("launch tab: %w", err)
	}
	root := result.Layout.Root
	created := CreatedTarget{Origin: Origin{h.Session, result.Layout.WorkspaceID, result.Layout.TabID, root.PaneID}, CreatedTab: true, DirectArgv: true}
	if result.Type != "layout_apply" || result.Layout.WorkspaceID != request.Origin.WorkspaceID || result.Layout.TabID == "" || result.Layout.TabID == request.Origin.TabID || root.Type != "pane" || root.PaneID == "" || root.CWD != request.Directory || !equalStrings(root.Command, request.Argv) || result.Layout.FocusedPaneID != root.PaneID {
		if plausibleCreatedTab(created, request.Origin) {
			return CreatedTarget{}, errors.Join(ErrStaleTarget, h.cleanupAfterFailure(ctx, created))
		}
		return CreatedTarget{}, ErrStaleTarget
	}
	return created, nil
}

type layoutPane struct {
	Type    string   `json:"type"`
	CWD     string   `json:"cwd"`
	Command []string `json:"command"`
}

func (h Host) launchPane(ctx context.Context, request LaunchRequest) (CreatedTarget, error) {
	splitArgs := []string{"pane", "split", "--pane", request.Origin.PaneID, "--direction", request.Direction, "--cwd", request.Directory}
	if request.OMPStatusSocket != "" {
		splitArgs = append(splitArgs, "--env", "HERDR_PLUGIN_KIT_OMP_STATUS_SOCKET="+request.OMPStatusSocket)
	}
	splitArgs = append(splitArgs, "--no-focus")
	args, err := h.sessionArgs(splitArgs)
	if err != nil {
		return CreatedTarget{}, fmt.Errorf("launch pane: %w", err)
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type string          `json:"type"`
			Pane json.RawMessage `json:"pane"`
		} `json:"result"`
	}
	if err := h.runJSON(ctx, args, &response); err != nil {
		return CreatedTarget{}, fmt.Errorf("launch pane: %w", err)
	}
	var pane wirePane
	decodeErr := json.Unmarshal(response.Result.Pane, &pane)
	created := CreatedTarget{Origin: Origin{h.Session, pane.WorkspaceID, pane.TabID, pane.PaneID}}
	if response.Result.Type != "pane_info" || decodeErr != nil || !samePaneLocation(pane, request.Origin.WorkspaceID, request.Origin.TabID) || pane.PaneID == request.Origin.PaneID || pane.CWD != request.Directory || pane.Focused {
		if decodeErr == nil && plausibleCreatedPane(created, request.Origin) {
			return CreatedTarget{}, errors.Join(ErrStaleTarget, h.cleanupAfterFailure(ctx, created))
		}
		return CreatedTarget{}, ErrStaleTarget
	}
	if err := h.waitForShell(ctx, created.Origin, request.Directory); err != nil {
		return CreatedTarget{}, errors.Join(fmt.Errorf("launch pane readiness: %w", err), h.cleanupAfterFailure(ctx, created))
	}
	command, err := encodeExec(request.Argv)
	if err != nil {
		return CreatedTarget{}, errors.Join(err, h.cleanupAfterFailure(ctx, created))
	}
	runArgs, err := h.sessionArgs([]string{"pane", "run", pane.PaneID, command})
	if err != nil {
		return CreatedTarget{}, errors.Join(err, h.cleanupAfterFailure(ctx, created))
	}
	var acknowledged bool
	if err := h.runJSON(ctx, runArgs, &acknowledged); err != nil || !acknowledged {
		if err == nil {
			err = errors.New("Herdr did not acknowledge pane command")
		}
		return CreatedTarget{}, errors.Join(fmt.Errorf("launch pane command: %w", err), h.cleanupAfterFailure(ctx, created))
	}
	return created, nil
}

// FocusPane verifies the exact session/workspace/tab/pane both before and
// after the focus request. This correlates the observed target with the issued
// intent; Herdr 0.8.2 cannot prove causation against a simultaneous identical
// external focus operation.
func (h Host) FocusPane(ctx context.Context, target Origin) error {
	if _, err := h.currentPane(ctx, target); err != nil {
		return fmt.Errorf("focus pane: %w", err)
	}
	socket, err := h.sessionSocket(ctx)
	if err != nil {
		return fmt.Errorf("focus pane: %w", err)
	}
	params := struct {
		PaneID string `json:"pane_id"`
	}{PaneID: target.PaneID}
	var result struct {
		Type string          `json:"type"`
		Pane json.RawMessage `json:"pane"`
	}
	if err := h.caller().Call(ctx, socket, "plugin-kit:pane.focus", "pane.focus", params, &result); err != nil {
		return fmt.Errorf("focus pane: %w", err)
	}
	var pane wirePane
	if result.Type != "pane_info" || json.Unmarshal(result.Pane, &pane) != nil || !samePaneLocation(pane, target.WorkspaceID, target.TabID) || pane.PaneID != target.PaneID || !pane.Focused {
		return fmt.Errorf("focus pane: %w", ErrStaleTarget)
	}
	after, err := h.currentPane(ctx, target)
	if err != nil || !after.Focused {
		return fmt.Errorf("focus pane: %w", ErrStaleTarget)
	}
	return nil
}

// Cleanup closes only the recorded task-created pane. Herdr removes an empty
// task-created tab itself; closing the pane instead of the whole tab also avoids
// disturbing any unrelated pane added to that tab after creation.
func (h Host) Cleanup(ctx context.Context, target CreatedTarget) error {
	if _, err := h.currentPane(ctx, target.Origin); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	args, err := h.sessionArgs([]string{"pane", "close", target.PaneID})
	if err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type string `json:"type"`
		} `json:"result"`
	}
	if err := h.runJSON(ctx, args, &response); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	if response.Result.Type != "ok" {
		return errors.New("cleanup: Herdr did not acknowledge pane close")
	}
	return nil
}

func (h Host) cleanupAfterFailure(parent context.Context, target CreatedTarget) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	return h.Cleanup(ctx, target)
}

func (h Host) currentPane(ctx context.Context, target Origin) (wirePane, error) {
	if err := h.validateOrigin(target); err != nil {
		return wirePane{}, err
	}
	args, err := h.sessionArgs([]string{"pane", "get", target.PaneID})
	if err != nil {
		return wirePane{}, err
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type string          `json:"type"`
			Pane json.RawMessage `json:"pane"`
		} `json:"result"`
	}
	if err := h.runJSON(ctx, args, &response); err != nil {
		return wirePane{}, ErrStaleTarget
	}
	var pane wirePane
	if response.Result.Type != "pane_info" || json.Unmarshal(response.Result.Pane, &pane) != nil || pane.PaneID != target.PaneID || !samePaneLocation(pane, target.WorkspaceID, target.TabID) {
		return wirePane{}, ErrStaleTarget
	}
	return pane, nil
}

func (h Host) waitForShell(parent context.Context, target Origin, directory string) error {
	ctx, cancel := context.WithTimeout(parent, ShellReadyTimeout)
	defer cancel()
	for {
		args, err := h.sessionArgs([]string{"pane", "process-info", "--pane", target.PaneID})
		if err != nil {
			return err
		}
		var response struct {
			ID     string `json:"id"`
			Result struct {
				Type        string          `json:"type"`
				ProcessInfo json.RawMessage `json:"process_info"`
			} `json:"result"`
		}
		if err := h.runJSON(ctx, args, &response); err != nil {
			return err
		}
		var info struct {
			PaneID                   string `json:"pane_id"`
			ShellPID                 int    `json:"shell_pid"`
			ForegroundProcessGroupID int    `json:"foreground_process_group_id"`
			ForegroundProcesses      []struct {
				PID  int      `json:"pid"`
				Name string   `json:"name"`
				CWD  string   `json:"cwd"`
				Argv []string `json:"argv"`
			} `json:"foreground_processes"`
		}
		if response.Result.Type != "pane_process_info" || json.Unmarshal(response.Result.ProcessInfo, &info) != nil || info.PaneID != target.PaneID || info.ShellPID <= 0 {
			return ErrStaleTarget
		}
		if info.ForegroundProcessGroupID == info.ShellPID && len(info.ForegroundProcesses) == 1 && info.ForegroundProcesses[0].PID == info.ShellPID {
			process := info.ForegroundProcesses[0]
			if process.CWD != directory {
				return ErrStaleTarget
			}
			if len(process.Argv) == 0 || !supportedShell(filepath.Base(process.Argv[0])) || process.Name != filepath.Base(process.Argv[0]) {
				return ErrUnsupportedShell
			}
			return nil
		}
		if err := h.waitInterval(ctx, 25*time.Millisecond); err != nil {
			return err
		}
	}
}

func (h Host) waitInterval(ctx context.Context, duration time.Duration) error {
	if h.wait != nil {
		return h.wait(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func supportedShell(name string) bool {
	switch name {
	case "sh", "bash", "dash", "zsh", "ksh", "mksh":
		return true
	default:
		return false
	}
}

func (h Host) validateLaunch(request LaunchRequest) error {
	if err := h.validateOrigin(request.Origin); err != nil {
		return err
	}
	if _, err := validateDirectory(request.Directory); err != nil {
		return err
	}
	if len(request.Argv) == 0 || len(request.Argv) > MaxArgv {
		return fmt.Errorf("argv must contain 1 to %d elements", MaxArgv)
	}
	for i, arg := range request.Argv {
		if strings.ContainsRune(arg, 0) || !utf8.ValidString(arg) || len(arg) > MaxArgBytes {
			return fmt.Errorf("argv[%d] must be valid UTF-8, NUL-free, and at most %d bytes", i, MaxArgBytes)
		}
	}
	if request.Argv[0] == "" || strings.HasPrefix(request.Argv[0], "-") {
		return errors.New("argv[0] must be nonempty and must not begin with a dash")
	}
	if request.OMPStatusSocket != "" {
		if filepath.Base(request.Argv[0]) != "omp" || !filepath.IsAbs(request.OMPStatusSocket) || filepath.Clean(request.OMPStatusSocket) != request.OMPStatusSocket || strings.ContainsRune(request.OMPStatusSocket, 0) {
			return errors.New("OMP status socket requires an OMP command and a clean absolute NUL-free path")
		}
	}
	if len(request.TabLabel) > 256 || strings.ContainsRune(request.TabLabel, 0) {
		return errors.New("tab label must be NUL-free and at most 256 bytes")
	}
	switch request.Placement {
	case NewTab:
		if request.Direction != "" {
			return errors.New("new-tab launch does not accept a split direction")
		}
	case NewPane:
		if request.Direction != "right" && request.Direction != "down" {
			return errors.New("new-pane launch direction must be right or down")
		}
	default:
		return errors.New("unsupported launch placement")
	}
	if request.Placement == NewPane {
		for i, arg := range request.Argv {
			for _, r := range arg {
				if unicode.IsControl(r) && r != '\n' {
					return fmt.Errorf("argv[%d] contains a control character unsupported by terminal delivery", i)
				}
			}
		}
		if _, err := encodeExec(request.Argv); err != nil {
			return err
		}
	}
	return nil
}

func (h Host) validateOrigin(origin Origin) error {
	if origin.SessionName != h.Session {
		return errors.New("Herdr session identity changed")
	}
	if h.Session != "" {
		if h.Session == "default" {
			return errors.New("the default Herdr session is represented by an empty session name")
		}
		if err := herdrid.ValidateSessionName(h.Session); err != nil {
			return err
		}
	}
	for name, value := range map[string]string{"workspace_id": origin.WorkspaceID, "tab_id": origin.TabID, "pane_id": origin.PaneID} {
		if value == "" || len(value) > 256 || strings.ContainsRune(value, 0) {
			return fmt.Errorf("%s must be nonempty, NUL-free, and at most 256 bytes", name)
		}
	}
	return nil
}

func (h Host) sessionArgs(args []string) ([]string, error) {
	if h.Session == "" {
		return args, nil
	}
	if h.Session == "default" {
		return nil, errors.New("the default Herdr session is represented by an empty session name")
	}
	if err := herdrid.ValidateSessionName(h.Session); err != nil {
		return nil, err
	}
	return append([]string{"--session", h.Session}, args...), nil
}

func (h Host) sessionSocket(ctx context.Context) (string, error) {
	sessions, err := h.List(ctx)
	if err != nil {
		return "", err
	}
	var matched *Session
	for i := range sessions {
		candidate := &sessions[i]
		isMatch := h.Session == "" && candidate.Default || h.Session != "" && !candidate.Default && candidate.Name == h.Session
		if !isMatch {
			continue
		}
		if matched != nil {
			return "", ErrAmbiguousTarget
		}
		matched = candidate
	}
	if matched == nil || !matched.Running {
		return "", ErrStaleTarget
	}
	if matched.SocketPath == "" || !filepath.IsAbs(matched.SocketPath) || filepath.Clean(matched.SocketPath) != matched.SocketPath || strings.ContainsRune(matched.SocketPath, 0) {
		return "", errors.New("invalid Herdr session socket path")
	}
	return matched.SocketPath, nil
}

func (h Host) caller() apiCaller {
	if h.api != nil {
		return h.api
	}
	return herdrapi.Client{}
}

func encodeExec(argv []string) (string, error) {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return "", fmt.Errorf("argv[%d] contains NUL", i)
		}
		parts[i] = "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
	}
	command := "exec " + strings.Join(parts, " ")
	if len(command) > MaxCommandLen {
		return "", errors.New("encoded command exceeds size limit")
	}
	return command, nil
}

func samePaneLocation(pane wirePane, workspaceID, tabID string) bool {
	return pane.PaneID != "" && pane.WorkspaceID == workspaceID && pane.TabID == tabID
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func launchEnv(request LaunchRequest) map[string]string {
	if request.OMPStatusSocket == "" {
		return nil
	}
	return map[string]string{"HERDR_PLUGIN_KIT_OMP_STATUS_SOCKET": request.OMPStatusSocket}
}

func plausibleCreatedTab(created CreatedTarget, origin Origin) bool {
	return created.SessionName == origin.SessionName && created.WorkspaceID == origin.WorkspaceID &&
		created.TabID != "" && created.TabID != origin.TabID && created.PaneID != ""
}

func plausibleCreatedPane(created CreatedTarget, origin Origin) bool {
	return created.SessionName == origin.SessionName && created.WorkspaceID == origin.WorkspaceID &&
		created.TabID == origin.TabID && created.PaneID != "" && created.PaneID != origin.PaneID
}

var _ apiCaller = herdrapi.Client{}
