//go:build linux

package shell

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

type ptySurface struct {
	layout     responsive.Layout
	generation uint64
	state      diagnostics.ID
}

func (surface *ptySurface) Update(_ EventContext, event Event) []Effect {
	switch event := event.(type) {
	case ResizeEvent:
		surface.layout, surface.generation = event.Layout, event.Generation
	case TextEvent:
		if event.Text == "q" {
			return []Effect{Quit()}
		}
	}
	return nil
}

func (surface *ptySurface) Render(context RenderContext) (*view.Frame, error) {
	frame, err := view.NewFrame(surface.layout.Render.Columns, surface.layout.Render.Rows)
	if err != nil {
		return nil, err
	}
	frame.PutText(0, 0, fmt.Sprintf("generation=%d size=%dx%d settled=%t", surface.generation, surface.layout.Reported.Columns, surface.layout.Reported.Rows, context.Settled), view.Style{})
	return frame, nil
}

func (surface *ptySurface) DiagnosticState() diagnostics.VisualState {
	return diagnostics.VisualState{
		State: surface.state,
		Geometry: diagnostics.Geometry{
			ReportedColumns: surface.layout.Reported.Columns, ReportedRows: surface.layout.Reported.Rows,
			RenderColumns: surface.layout.Render.Columns, RenderRows: surface.layout.Render.Rows,
		},
		ResizeGeneration: surface.generation,
	}
}

func TestShellHelperProcess(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_SHELL_HELPER") != "1" {
		return
	}
	directory := os.Getenv("HERDR_PLUGIN_KIT_DIAGNOSTICS")
	recorder, err := diagnostics.Open(diagnostics.DefaultConfig(directory))
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	palette, _ := theme.Builtin("terminal")
	state, _ := diagnostics.NewID("ready")
	pluginID, _ := diagnostics.NewID("pty-test")
	program, err := NewProgram(ProgramOptions{
		PluginID: pluginID, Theme: palette, Events: recorder, Input: os.Stdin, Output: os.Stdout,
	}, &ptySurface{state: state})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := program.Run(); err != nil {
		t.Fatal(err)
	}
}

type lockedBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.data.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.data.String()
}

func (buffer *lockedBuffer) Len() int {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.data.Len()
}

func (buffer *lockedBuffer) stringAfter(offset int) string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	data := buffer.data.Bytes()
	if offset > len(data) {
		offset = len(data)
	}
	return string(data[offset:])
}

func (buffer *lockedBuffer) containsAfter(offset int, value string) bool {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	data := buffer.data.Bytes()
	if offset > len(data) {
		offset = len(data)
	}
	return bytes.Contains(data[offset:], []byte(value))
}

func TestPTYResizeOutputAndSettlementTiming(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_TIMING") != "1" {
		t.Skip("run make test-timing for isolated wall-clock verification")
	}
	if raceEnabled || testing.CoverMode() != "" {
		t.Skip("wall-clock latency is measured without instrumentation")
	}
	directory := t.TempDir()
	master, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer terminal.Close()
	if err := pty.Setsize(master, &pty.Winsize{Cols: 70, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	before := terminalState(t, terminal.Name())
	command := exec.Command(os.Args[0], "-test.run=^TestShellHelperProcess$")
	command.Env = append(os.Environ(),
		"HERDR_PLUGIN_KIT_SHELL_HELPER=1",
		"HERDR_PLUGIN_KIT_DIAGNOSTICS="+directory,
	)
	command.Stdin, command.Stdout, command.Stderr = terminal, terminal, terminal
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var output lockedBuffer
	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&output, master)
		close(copyDone)
	}()
	waitForOutputAfter(t, &output, 0, "generation=1 size=70x30")

	randomRows := []uint16{10, 13, 30, 66}
	durations := make([]time.Duration, 0, 200)
	settleDurations := make([]time.Duration, 0, 200)
	observedSettleDurations := make([]time.Duration, 0, 200)
	persistedSettleDurations := make([]time.Duration, 0, 200)
	generation := uint64(1)
	var logOffset int64
	for trace := range 200 {
		for step := range 3 {
			generation++
			rows := randomRows[(trace+step)%len(randomRows)]
			offset := output.Len()
			setPTYSize(t, command, master, 70, rows)
			waitForOutputAfter(t, &output, offset, fmt.Sprintf("generation=%d size=70x%d", generation, rows))
		}
		generation++
		offset := output.Len()
		finalStarted := time.Now()
		setPTYSize(t, command, master, 500, 200)
		waitForOutputAfter(t, &output, offset, fmt.Sprintf("generation=%d size=500x200 settled=false", generation))
		durations = append(durations, time.Since(finalStarted))
		settleDuration, persistedAt, observedAt, nextOffset := waitForSettledAfter(t, directory, generation, logOffset)
		logOffset = nextOffset
		observedSettleDuration := observedAt.Sub(finalStarted)
		persistedSettleDuration := persistedAt.Sub(finalStarted.Round(0))
		settleDurations = append(settleDurations, settleDuration)
		observedSettleDurations = append(observedSettleDurations, observedSettleDuration)
		persistedSettleDurations = append(persistedSettleDurations, persistedSettleDuration)
		if settleDuration < 100*time.Millisecond || settleDuration > 250*time.Millisecond {
			t.Fatalf("trace %d final settle = %s (persisted wall time %s, observed at %s), want 100ms..250ms", trace, settleDuration, persistedSettleDuration, observedSettleDuration)
		}
		waitForOutputAfter(t, &output, offset, fmt.Sprintf("generation=%d size=500x200 settled=true", generation))
		waitForOutputQuiescence(t, &output)
		assertNoObsoleteGenerationAfterFinal(t, output.stringAfter(offset), generation)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95Output := durations[(len(durations)*95+99)/100-1]
	minSettle, maxSettle := settleDurations[0], settleDurations[0]
	minObservedSettle, maxObservedSettle := observedSettleDurations[0], observedSettleDurations[0]
	minPersistedSettle, maxPersistedSettle := persistedSettleDurations[0], persistedSettleDurations[0]
	for _, duration := range settleDurations[1:] {
		minSettle = min(minSettle, duration)
		maxSettle = max(maxSettle, duration)
	}
	for _, duration := range observedSettleDurations[1:] {
		minObservedSettle = min(minObservedSettle, duration)
		maxObservedSettle = max(maxObservedSettle, duration)
	}
	for _, duration := range persistedSettleDurations[1:] {
		minPersistedSettle = min(minPersistedSettle, duration)
		maxPersistedSettle = max(maxPersistedSettle, duration)
	}
	evidence, _ := json.Marshal(map[string]any{
		"traces": 200, "seed": "rows-10-13-30-66", "p95_pty_ns": p95Output.Nanoseconds(),
		"min_settle_ns": minSettle.Nanoseconds(), "max_settle_ns": maxSettle.Nanoseconds(),
		"min_observed_settle_ns": minObservedSettle.Nanoseconds(), "max_observed_settle_ns": maxObservedSettle.Nanoseconds(),
		"min_persisted_settle_ns": minPersistedSettle.Nanoseconds(), "max_persisted_settle_ns": maxPersistedSettle.Nanoseconds(),
	})
	t.Log(string(evidence))
	if p95Output > 100*time.Millisecond {
		t.Fatalf("p95 final resize-to-PTY = %s, want <= 100ms", p95Output)
	}
	_, _ = master.Write([]byte("q"))
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case err := <-wait:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("shell helper did not exit")
	}
	after := terminalState(t, terminal.Name())
	if before != after {
		t.Fatalf("terminal state changed: before %q after %q", before, after)
	}
	data := []byte(output.String())
	assertModeBalanced(t, data, []byte("\x1b[?1049h"), []byte("\x1b[?1049l"), "alternate screen")
	assertModeBalanced(t, data, []byte("\x1b[?2004h"), []byte("\x1b[?2004l"), "bracketed paste")
	select {
	case <-copyDone:
	default:
	}
}

func setPTYSize(t *testing.T, command *exec.Cmd, master *os.File, columns, rows uint16) {
	t.Helper()
	if err := pty.Setsize(master, &pty.Winsize{Cols: columns, Rows: rows}); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Signal(syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
}

func waitForOutputAfter(t *testing.T, output *lockedBuffer, offset int, value string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if output.containsAfter(offset, value) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("missing output %q", value)
}

func waitForOutputQuiescence(t *testing.T, output *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	lastLength := output.Len()
	quietSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		length := output.Len()
		if length != lastLength {
			lastLength = length
			quietSince = time.Now()
			continue
		}
		if time.Since(quietSince) >= 3*time.Millisecond {
			return
		}
	}
	t.Fatal("PTY output did not quiesce")
}

func assertNoObsoleteGenerationAfterFinal(t *testing.T, output string, want uint64) {
	t.Helper()
	const marker = "generation="
	finalMarker := fmt.Sprintf("%s%d ", marker, want)
	finalIndex := strings.Index(output, finalMarker)
	if finalIndex < 0 {
		t.Fatalf("final generation %d disappeared", want)
	}
	output = output[finalIndex:]
	found := false
	for {
		index := strings.Index(output, marker)
		if index < 0 {
			break
		}
		output = output[index+len(marker):]
		end := 0
		for end < len(output) && output[end] >= '0' && output[end] <= '9' {
			end++
		}
		generation, err := strconv.ParseUint(output[:end], 10, 64)
		if err != nil {
			t.Fatalf("invalid generation marker %q", output[:end])
		}
		found = true
		if generation != want {
			t.Fatalf("generation %d rendered during final generation %d", generation, want)
		}
		output = output[end:]
	}
	if !found {
		t.Fatalf("final generation %d marker could not be parsed", want)
	}
}

func waitForSettledAfter(t *testing.T, directory string, generation uint64, offset int64) (time.Duration, time.Time, time.Time, int64) {
	t.Helper()
	path := filepath.Join(directory, diagnostics.EventLogName)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		file, err := os.Open(path)
		if err != nil {
			time.Sleep(time.Millisecond)
			continue
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			file.Close()
			t.Fatal(err)
		}
		data, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		consumed := int64(0)
		for {
			newline := bytes.IndexByte(data, '\n')
			if newline < 0 {
				break
			}
			line := data[:newline]
			data = data[newline+1:]
			consumed += int64(newline + 1)
			var event diagnostics.Event
			if json.Unmarshal(line, &event) != nil || event.Message != "resize.settled" {
				continue
			}
			value, currentGeneration := event.Details["generation"].(float64)
			related, originatingGeneration := event.Details["related_generation"].(float64)
			if currentGeneration && uint64(value) == generation && originatingGeneration && uint64(related) == generation &&
				event.Action == "resize.settle" && event.Details["outcome"] == string(diagnostics.OutcomeApplied) {
				duration, ok := event.Details["duration_ns"].(float64)
				if !ok || duration < 0 {
					t.Fatalf("settled event for generation %d has invalid duration %#v", generation, event.Details["duration_ns"])
				}
				return time.Duration(duration), event.Time, time.Now(), offset + consumed
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("missing settled event for generation %d", generation)
	return 0, time.Time{}, time.Time{}, offset
}

func terminalState(t *testing.T, path string) string {
	t.Helper()
	output, err := exec.Command("stty", "-g", "-F", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func assertModeBalanced(t *testing.T, output, enter, leave []byte, name string) {
	t.Helper()
	entered, left := bytes.Count(output, enter), bytes.Count(output, leave)
	if entered == 0 || entered != left {
		t.Fatalf("%s enter=%d leave=%d", name, entered, left)
	}
}
