//go:build linux

package shell

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

type inputActivityPTYSurface struct{ *observerSurface }

func (s *inputActivityPTYSurface) Render(c RenderContext) (*view.Frame, error) {
	f, e := view.NewFrame(c.Layout.Render.Columns, c.Layout.Render.Rows)
	if e != nil {
		return nil, e
	}
	f.PutText(0, 0, "observer-ready", view.Style{})
	if s.captured > 0 {
		f.PutText(0, 1, fmt.Sprintf("later=%d", *s.generation-s.captured), view.Style{})
	}
	return f, nil
}
func TestInputActivityTTYHelper(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_INPUT_ACTIVITY_TTY") != "1" {
		return
	}
	_, base, sink := mappedModel(t)
	var generation uint64
	s := &inputActivityPTYSurface{observerSurface: &observerSurface{mappedSurface: base, generation: &generation}}
	runtime, e := keymap.New(s.KeymapCatalog())
	if e != nil {
		t.Fatal(e)
	}
	palette, _ := theme.Builtin("terminal")
	program, e := NewProgram(ProgramOptions{PluginID: testID(t, "tty-observer"), Theme: palette, Events: sink, Keymap: runtime, ObserveInputActivity: func() { generation++ }}, s)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = program.Run(); e != nil {
		t.Fatal(e)
	}
	if s.captured == 0 || generation < s.captured+3 {
		t.Fatalf("default TTY lost trailing input: captured=%d generation=%d", s.captured, generation)
	}
	for _, event := range sink.snapshot() {
		if event.Bytes != 0 && event.Code.String() == "input.text" {
			t.Fatal("diagnostics retained raw text")
		}
	}
}

func TestInputActivityActualTTY(t *testing.T) {
	for _, test := range []struct {
		name       string
		redirected bool
		quit       byte
	}{
		{"default stdin quit", false, 'q'},
		{"default stdin Ctrl+C", false, 3},
		{"redirected stdin quit", true, 'q'},
		{"redirected stdin Ctrl+C", true, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			master, terminal, e := pty.Open()
			if e != nil {
				t.Fatal(e)
			}
			defer master.Close()
			defer terminal.Close()
			if e = pty.Setsize(master, &pty.Winsize{Cols: 80, Rows: 18}); e != nil {
				t.Fatal(e)
			}
			before := terminalState(t, terminal.Name())
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInputActivityTTYHelper$")
			cmd.Env = append(os.Environ(), "HERDR_PLUGIN_KIT_INPUT_ACTIVITY_TTY=1")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = terminal, terminal, terminal
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if test.redirected {
				null, e := os.Open(os.DevNull)
				if e != nil {
					t.Fatal(e)
				}
				defer null.Close()
				cmd.Stdin = null
				cmd.ExtraFiles = []*os.File{terminal}
				cmd.SysProcAttr.Ctty = 3
			}
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			var output lockedBuffer
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("TTY setup output: %q", output.String())
				}
			})
			go func() { _, _ = io.Copy(&output, master) }()
			// Bubble Tea queries the terminal during package initialization,
			// before the helper or shell can run. Supply the terminal response.
			waitForOutputAfter(t, &output, 0, "\x1b[6n")
			_, _ = master.Write([]byte("\x1b]11;rgb:0000/0000/0000\x1b\\\x1b[1;1R"))
			waitForOutputAfter(t, &output, 0, "observer-ready")
			_, _ = master.Write([]byte("mzz"))
			waitForOutputAfter(t, &output, 0, "later=2")
			_, _ = master.Write([]byte{test.quit})
			if e = cmd.Wait(); e != nil {
				t.Fatalf("TTY helper: %v %s", e, output.String())
			}
			after := terminalState(t, terminal.Name())
			if before != after {
				t.Fatal("terminal settings not restored")
			}
		})
	}
}
