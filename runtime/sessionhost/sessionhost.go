// Package sessionhost wraps supported Herdr 0.8.2 session operations.
package sessionhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

const MaxJSONBytes = 1 << 20

var sessionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type Host struct {
	Runner command.Runner
	Herdr  string
}

type Capabilities struct {
	List            bool
	Stop            bool
	Delete          bool
	OpenAtDirectory bool
	OneCommandCWD   bool
	ShortcutBinding bool
}

func SupportedCapabilities() Capabilities {
	return Capabilities{List: true, Stop: true, Delete: true, OpenAtDirectory: true}
}

type Session struct {
	Name       string `json:"name"`
	Default    bool   `json:"default"`
	Running    bool   `json:"running"`
	SessionDir string `json:"session_dir"`
	SocketPath string `json:"socket_path"`
}

type Workspace struct {
	WorkspaceID string            `json:"workspace_id"`
	Number      uint              `json:"number"`
	Label       string            `json:"label"`
	Focused     bool              `json:"focused"`
	PaneCount   uint              `json:"pane_count"`
	TabCount    uint              `json:"tab_count"`
	ActiveTabID string            `json:"active_tab_id"`
	AgentStatus string            `json:"agent_status"`
	Tokens      map[string]string `json:"tokens,omitempty"`
	Worktree    json.RawMessage   `json:"worktree,omitempty"`
}

func (h Host) List(ctx context.Context) ([]Session, error) {
	var response struct {
		Sessions []Session `json:"sessions"`
	}
	if err := h.runJSON(ctx, []string{"session", "list", "--json"}, &response); err != nil {
		return nil, err
	}
	for i, session := range response.Sessions {
		if err := validateName(session.Name); err != nil {
			return nil, fmt.Errorf("sessions[%d]: %w", i, err)
		}
	}
	return response.Sessions, nil
}

func (h Host) Stop(ctx context.Context, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	var response struct {
		Status  string `json:"status"`
		Session string `json:"session"`
	}
	if err := h.runJSON(ctx, []string{"session", "stop", name, "--json"}, &response); err != nil {
		return err
	}
	if response.Status != "stopped" || response.Session != name {
		return errors.New("unexpected session stop response")
	}
	return nil
}

func (h Host) Delete(ctx context.Context, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if name == "default" {
		return errors.New("deleting the default session is unsupported")
	}
	var response struct {
		Status    string `json:"status"`
		Session   string `json:"session"`
		Directory string `json:"directory"`
	}
	if err := h.runJSON(ctx, []string{"session", "delete", name, "--json"}, &response); err != nil {
		return err
	}
	if response.Status != "deleted" || response.Session != name {
		return errors.New("unexpected session delete response")
	}
	return nil
}

// OpenAtDirectory is deliberately the proven two-command composition. Herdr
// 0.8.2 has no supported one-command named-session cwd creation operation.
func (h Host) OpenAtDirectory(ctx context.Context, name, directory string) (Workspace, error) {
	if err := validateName(name); err != nil {
		return Workspace{}, err
	}
	directory, err := validateDirectory(directory)
	if err != nil {
		return Workspace{}, err
	}
	if err := h.run(ctx, []string{"--session", name}); err != nil {
		return Workspace{}, fmt.Errorf("start named session: %w", err)
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type      string          `json:"type"`
			Workspace Workspace       `json:"workspace"`
			Tab       json.RawMessage `json:"tab"`
			RootPane  json.RawMessage `json:"root_pane"`
		} `json:"result"`
	}
	if err := h.runJSON(ctx, []string{"--session", name, "workspace", "create", "--cwd", directory}, &response); err != nil {
		return Workspace{}, fmt.Errorf("create workspace: %w", err)
	}
	if response.Result.Type != "workspace_created" || response.Result.Workspace.WorkspaceID == "" {
		return Workspace{}, errors.New("unexpected workspace create response")
	}
	return response.Result.Workspace, nil
}

func (h Host) run(ctx context.Context, args []string) error {
	if h.Runner == nil {
		return errors.New("session host requires a command runner")
	}
	executable := h.Herdr
	if executable == "" {
		executable = "herdr"
	}
	result, err := h.Runner.Run(ctx, executable, args)
	if err != nil {
		return err
	}
	if result.StdoutTruncated || result.StderrTruncated {
		return errors.New("Herdr command output was truncated")
	}
	return nil
}

func (h Host) runJSON(ctx context.Context, args []string, target any) error {
	if h.Runner == nil {
		return errors.New("session host requires a command runner")
	}
	executable := h.Herdr
	if executable == "" {
		executable = "herdr"
	}
	result, err := h.Runner.Run(ctx, executable, args)
	if err != nil {
		return err
	}
	if result.StdoutTruncated || result.StderrTruncated || len(result.Stdout) > MaxJSONBytes {
		return errors.New("Herdr command output exceeded bounds")
	}
	dec := json.NewDecoder(bytes.NewReader(result.Stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("decode Herdr JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("Herdr output must contain exactly one JSON value")
	}
	return nil
}

func validateName(name string) error {
	if !sessionName.MatchString(name) || name == "." || name == ".." {
		return errors.New("invalid session name")
	}
	return nil
}

func validateDirectory(directory string) (string, error) {
	if directory == "" || len(directory) > 4096 || strings.ContainsRune(directory, 0) || !filepath.IsAbs(directory) {
		return "", errors.New("directory must be an absolute NUL-free path of at most 4096 bytes")
	}
	clean := filepath.Clean(directory)
	if clean != directory {
		return "", errors.New("directory must be clean and absolute")
	}
	return clean, nil
}
