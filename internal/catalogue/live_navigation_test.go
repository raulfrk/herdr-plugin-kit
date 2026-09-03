package catalogue

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

func TestLiveAxisShortcutsMoveInTheirDocumentedDirection(t *testing.T) {
	themes := theme.IDs()
	tests := []struct {
		name          string
		key           string
		wantFocus     string
		wantScreen    string
		wantSelection string
		wantState     string
	}{
		{name: "design forward", key: "d", wantFocus: "design", wantScreen: "catalogue.recall-search", wantSelection: liveDesigns[1].ID + "." + treatments[0].ID + "." + themes[0], wantState: "query.live.plain"},
		{name: "design reverse", key: "D", wantFocus: "design", wantScreen: "catalogue.recall-search", wantSelection: liveDesigns[len(liveDesigns)-1].ID + "." + treatments[0].ID + "." + themes[0], wantState: "query.live.plain"},
		{name: "treatment forward", key: "e", wantFocus: "design", wantScreen: "catalogue.recall-search", wantSelection: liveDesigns[0].ID + "." + treatments[1].ID + "." + themes[0], wantState: "query.live.plain"},
		{name: "treatment reverse", key: "E", wantFocus: "design", wantScreen: "catalogue.recall-search", wantSelection: liveDesigns[0].ID + "." + treatments[len(treatments)-1].ID + "." + themes[0], wantState: "query.live.plain"},
		{name: "plugin forward", key: "p", wantFocus: "design", wantScreen: "catalogue.attention-switcher", wantSelection: liveDesigns[0].ID + "." + treatments[0].ID + "." + themes[0], wantState: "agents.live.plain"},
		{name: "plugin reverse", key: "P", wantFocus: "design", wantScreen: "catalogue.debug-ui", wantSelection: liveDesigns[0].ID + "." + treatments[0].ID + "." + themes[0], wantState: "health.live.plain"},
		{name: "state forward", key: "s", wantFocus: "design", wantScreen: "catalogue.recall-search", wantSelection: liveDesigns[0].ID + "." + treatments[0].ID + "." + themes[0], wantState: "results.live.plain"},
		{name: "state reverse", key: "S", wantFocus: "design", wantScreen: "catalogue.recall-search", wantSelection: liveDesigns[0].ID + "." + treatments[0].ID + "." + themes[0], wantState: "error.live.plain"},
		{name: "theme forward", key: "t", wantFocus: "design", wantScreen: "catalogue.recall-search", wantSelection: liveDesigns[0].ID + "." + treatments[0].ID + "." + themes[1], wantState: "query.live.plain"},
		{name: "theme reverse", key: "T", wantFocus: "design", wantScreen: "catalogue.recall-search", wantSelection: liveDesigns[0].ID + "." + treatments[0].ID + "." + themes[len(themes)-1], wantState: "query.live.plain"},
		{name: "fixture forward", key: "f", wantFocus: "design", wantScreen: "catalogue.recall-search", wantSelection: liveDesigns[0].ID + "." + treatments[0].ID + "." + themes[0], wantState: "query.recovery-width.plain"},
		{name: "fixture reverse", key: "F", wantFocus: "design", wantScreen: "catalogue.recall-search", wantSelection: liveDesigns[0].ID + "." + treatments[0].ID + "." + themes[0], wantState: "query.projected.plain"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			surface := NewLiveSurface()
			surface.Update(shell.EventContext{}, shell.TextEvent{Text: test.key})
			got := surface.DiagnosticState()
			if got.Focus.String() != test.wantFocus || got.Screen.String() != test.wantScreen ||
				got.Selection.String() != test.wantSelection || got.State.String() != test.wantState {
				t.Fatalf("shortcut %q diagnostics = %s, want %s|%s|%s|%s", test.key,
					diagnosticKey(got), test.wantScreen, test.wantFocus, test.wantSelection, test.wantState)
			}
		})
	}
}

func TestLiveTabAndArrowNavigationExposeEveryAxis(t *testing.T) {
	surface := NewLiveSurface()
	wantFocus := []string{"design", "treatment", "plugin", "state", "theme", "fixture", "design"}
	if got := surface.DiagnosticState().Focus.String(); got != wantFocus[0] {
		t.Fatalf("initial focus = %q, want %q", got, wantFocus[0])
	}
	for _, want := range wantFocus[1:] {
		surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyTab})
		if got := surface.DiagnosticState().Focus.String(); got != want {
			t.Fatalf("focus after Tab = %q, want %q", got, want)
		}
	}

	for _, test := range []struct {
		name, focus, before, afterRight, afterLeft string
	}{
		{name: "design", focus: "design", before: liveDesigns[0].ID, afterRight: liveDesigns[1].ID, afterLeft: liveDesigns[0].ID},
		{name: "treatment", focus: "treatment", before: treatments[0].ID, afterRight: treatments[1].ID, afterLeft: treatments[0].ID},
		{name: "plugin", focus: "plugin", before: "catalogue.recall-search", afterRight: "catalogue.attention-switcher", afterLeft: "catalogue.recall-search"},
		{name: "state", focus: "state", before: "query.live.plain", afterRight: "results.live.plain", afterLeft: "query.live.plain"},
		{name: "theme", focus: "theme", before: theme.IDs()[0], afterRight: theme.IDs()[1], afterLeft: theme.IDs()[0]},
		{name: "fixture", focus: "fixture", before: "query.live.plain", afterRight: "query.recovery-width.plain", afterLeft: "query.live.plain"},
	} {
		t.Run(test.name, func(t *testing.T) {
			surface := NewLiveSurface()
			for surface.DiagnosticState().Focus.String() != test.focus {
				surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyTab})
			}
			observable := func() string {
				state := surface.DiagnosticState()
				switch test.focus {
				case "design":
					return strings.Split(state.Selection.String(), ".")[0]
				case "treatment":
					return strings.Split(state.Selection.String(), ".")[1]
				case "plugin":
					return state.Screen.String()
				case "theme":
					return strings.Split(state.Selection.String(), ".")[2]
				default:
					return state.State.String()
				}
			}
			if got := observable(); got != test.before {
				t.Fatalf("initial %s value = %q, want %q", test.focus, got, test.before)
			}
			surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyRight})
			if got := observable(); got != test.afterRight {
				t.Fatalf("%s after Right = %q, want %q", test.focus, got, test.afterRight)
			}
			surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyLeft})
			if got := observable(); got != test.afterLeft {
				t.Fatalf("%s after Left = %q, want %q", test.focus, got, test.afterLeft)
			}
		})
	}
}

func TestLivePluginNavigationResetsPluginLocalInteractionState(t *testing.T) {
	for _, navigation := range []struct {
		name                  string
		prepare               func(*LiveSurface)
		move                  func(*LiveSurface)
		whileEditing          bool
		wantScreen, wantState string
	}{
		{name: "shortcut forward", move: func(s *LiveSurface) { s.Update(shell.EventContext{}, shell.TextEvent{Text: "p"}) }, wantScreen: "catalogue.attention-switcher", wantState: "agents.live.plain"},
		{name: "shortcut reverse", move: func(s *LiveSurface) { s.Update(shell.EventContext{}, shell.TextEvent{Text: "P"}) }, wantScreen: "catalogue.debug-ui", wantState: "health.live.plain"},
		{name: "arrow right while editing", prepare: focusPluginAxis, move: func(s *LiveSurface) { s.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyRight}) }, whileEditing: true, wantScreen: "catalogue.attention-switcher", wantState: "agents.live.plain"},
		{name: "arrow left while editing", prepare: focusPluginAxis, move: func(s *LiveSurface) { s.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyLeft}) }, whileEditing: true, wantScreen: "catalogue.debug-ui", wantState: "health.live.plain"},
	} {
		t.Run(navigation.name, func(t *testing.T) {
			surface := NewLiveSurface()
			surface.Update(shell.EventContext{}, shell.TextEvent{Text: "S"})
			for range 3 {
				surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
			}
			surface.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
			surface.Update(shell.EventContext{}, shell.TextEvent{Text: "stale query"})
			if !navigation.whileEditing {
				surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
			}
			if navigation.prepare != nil {
				navigation.prepare(surface)
			}

			navigation.move(surface)
			got := surface.DiagnosticState()
			if got.Screen.String() != navigation.wantScreen || got.State.String() != navigation.wantState || got.SelectedIndex != 1 || got.Pending {
				t.Fatalf("after plugin navigation diagnostics = %+v, want screen=%s state=%s selected=1 pending=false", got, navigation.wantScreen, navigation.wantState)
			}
			plain := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 24}, false)
			if strings.Contains(plain, "stale query") {
				t.Fatalf("plugin navigation retained the previous query:\n%s", plain)
			}
			if !strings.Contains(plain, surfaceBase(strings.TrimPrefix(navigation.wantScreen, "catalogue.")).query) {
				t.Fatalf("plugin navigation did not restore the destination's default query:\n%s", plain)
			}
		})
	}
}

func TestLiveUpAndDownWrapTheVisibleSelection(t *testing.T) {
	surface := NewLiveSurface()
	assertSelection := func(wantIndex int, wantRow string) {
		t.Helper()
		if got := surface.DiagnosticState().SelectedIndex; got != wantIndex {
			t.Fatalf("selected index = %d, want %d", got, wantIndex)
		}
		plain := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 24}, false)
		if !strings.Contains(plain, "▌ "+wantRow) {
			t.Fatalf("selected row %q is not visibly marked:\n%s", wantRow, plain)
		}
	}

	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyUp})
	assertSelection(0, surfaceBase("recall-search").rows[0])
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyUp})
	assertSelection(5, surfaceBase("recall-search").rows[5])
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	assertSelection(0, surfaceBase("recall-search").rows[0])
}

func TestLiveEmptyQueryEditingAlwaysShowsTheCursor(t *testing.T) {
	surface := NewLiveSurface()
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	if got := surface.DiagnosticState(); !got.Pending {
		t.Fatalf("slash did not enter query editing: %+v", got)
	}
	if plain := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 24}, false); !strings.Contains(plain, "⌕  ▏") {
		t.Fatalf("empty editing query omits cursor:\n%s", plain)
	}

	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	if plain := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 24}, false); !strings.Contains(plain, "⌕  ▏") {
		t.Fatalf("backspace on an empty query removed the editing cursor:\n%s", plain)
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "needle"})
	if plain := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 24}, false); !strings.Contains(plain, "⌕  needle▏") {
		t.Fatalf("non-empty editing query omits trailing cursor:\n%s", plain)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	plain := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 24}, false)
	if strings.Contains(plain, "needle▏") || !strings.Contains(plain, "⌕  needle") || surface.DiagnosticState().Pending {
		t.Fatalf("committed query cursor/pending state is wrong:\n%s", plain)
	}
}

func TestLiveTitleAndStatusSwitchFromCompactAtNineteenRows(t *testing.T) {
	surface := NewLiveSurface()
	compact := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 18}, false)
	for _, want := range []string{"D1 Air · E1 Structured · P1 Recall", "S1 Query · T1 " + theme.IDs()[0] + " · F1 Live terminal"} {
		if !strings.Contains(compact, want) {
			t.Errorf("18-row render omits compact text %q:\n%s", want, compact)
		}
	}
	if strings.Contains(compact, "Recall Search · Bento Air / Structured") {
		t.Errorf("18-row render unexpectedly uses the full title:\n%s", compact)
	}

	full := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 19}, false)
	for _, want := range []string{"Recall Search · Bento Air / Structured", "Query · " + theme.IDs()[0] + " · Live terminal", "? help"} {
		if !strings.Contains(full, want) {
			t.Errorf("19-row render omits full text %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "D1 Air") || strings.Contains(full, "S1 Query") {
		t.Errorf("19-row render retained compact title/status:\n%s", full)
	}
}

func TestLiveHelpIsDiscoverableAndReadableAtMinimumSize(t *testing.T) {
	surface := NewLiveSurface()
	plain := renderLiveText(t, surface, responsive.Size{Columns: 40, Rows: 10}, false)
	if !strings.Contains(plain, "?:help  Tab:field  ←→:change  ↑↓:pick") {
		t.Fatalf("compact navigation footer is missing or clipped:\n%s", plain)
	}

	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "?"})
	help := renderLiveText(t, surface, responsive.Size{Columns: 40, Rows: 10}, false)
	for _, want := range []string{
		"CATALOGUE HELP", "d/D design · e/E treatment", "p/P plugin · s/S state",
		"t/T theme · f/F fixture", "Tab field · ←→ change", "↑↓ select result",
		"/ search · Backspace delete/back", "Enter next / commit search",
		"h HUD · ? / Esc close help", "q / Ctrl-C quit",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("minimum-size help omits %q:\n%s", want, help)
		}
	}
	if got := surface.DiagnosticState().State.String(); !strings.HasSuffix(got, ".help") {
		t.Errorf("help diagnostics state = %q", got)
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "h"})
	if got := surface.DiagnosticState().State.String(); !strings.HasSuffix(got, ".help-hud") {
		t.Errorf("help + HUD diagnostics state = %q", got)
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "h"})
	if effects := surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEscape}); len(effects) != 0 || surface.help {
		t.Fatalf("Esc did not close help without quitting: help=%t effects=%v", surface.help, effects)
	}
}

func TestBentoDesignsHaveStableSemanticLayouts(t *testing.T) {
	want := map[string]string{
		"bento-air/40x10":      "2137dc4d14f8649fcb558fcc0051b8d75c18bcdaff4dd2023f241715e9d5d3a1",
		"bento-command/40x10":  "e034f3bb45d866347bbc6c2d72cfa2190c138a361351689df67e5cc6403b2790",
		"bento-flow/40x10":     "3190b5f0d64c467156d067e07c22fb60f2b518436ebe152a3b6c67c7bec4b0ac",
		"bento-air/48x30":      "be53a228326e5470b93ed22160f96516783615d7a0b3d4bff0561c9f81c7df31",
		"bento-command/48x30":  "43b57e09aaefee7b8ece4e3cc01984690b7ebeae78a4a4da753b3cac11d5f992",
		"bento-flow/48x30":     "c52bb0e6464acf1ea1b52f5de328af67312581afd79b056b64a9fac43315f2ac",
		"bento-air/110x24":     "2c92ace4ebbc2f9aa26306b045c69a93c5e9bfa06ebcb88e4b7ffdbdec8f7a44",
		"bento-command/110x24": "a1ab0fe4457031db7e54ac5890cba26f98b5f900cca48828d1c885c4e8f22a4e",
		"bento-flow/110x24":    "a5dd8e56dc5856b4e3e2d0ddc4e384c83a4cf1c3d3f3b44eeadbeb45ac83ea57",
	}
	for _, size := range []responsive.Size{{Columns: 40, Rows: 10}, {Columns: 48, Rows: 30}, {Columns: 110, Rows: 24}} {
		for design := range liveDesigns {
			surface := NewLiveSurface()
			surface.design = design
			layout := responsive.Resolve(size)
			frame, err := surface.Render(shell.RenderContext{Layout: layout, Settled: true})
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%s/%dx%d", liveDesigns[design].ID, size.Columns, size.Rows)
			if got := semanticFrameDigest(frame); got != want[key] {
				t.Errorf("%s semantic frame = %s", key, got)
			}
		}
	}
}

func TestBentoDesignsHaveStableLongContentLayouts(t *testing.T) {
	want := map[string]string{
		"bento-air/40x10":      "9ba9a0f77748b6b1169082166f2bfeeb362027046bafdafd470cda4643065749",
		"bento-command/40x10":  "65e34da8be3f029060b9b640bb25e8a75dc9cf61ddd50d4c948773d05d6501ab",
		"bento-flow/40x10":     "dd3d5c40a9250869e8c5c67aec398a7f104bf169ae0980db62f78a35ac01d79e",
		"bento-air/48x30":      "6039ac68287bdd8295c34c47cf054161ac2ff1293329ae68ef5a8d3e7383e28a",
		"bento-command/48x30":  "49e489fcac5308e30efe7ffb561653f59f3ebfe598963685376f3bf8fac25a3f",
		"bento-flow/48x30":     "d3bbea08e322ec17e8cefa4be62dba7bde8a978d96f9145f2d42acf6bc9c9bfc",
		"bento-air/110x24":     "874e4cd41be3eca8a4e9657a63bb6c115fc5047b79ec7dbae3a36ce58db9fcbe",
		"bento-command/110x24": "7ef28734b9363fc32dc56c964a59d5ea215aec9484770c612763fb5b8a9f246e",
		"bento-flow/110x24":    "7f4ed4b1b578e7eafc2c4ba2349e2f3f196dbf87b165524246b09b275543f721",
	}
	long := strings.Repeat("wide content ", 12)
	data := sample{
		title: long, query: long, status: long, help: long, selected: 4,
		rows:   []string{long + "0", long + "1", long + "2", long + "3", long + "4", long + "5"},
		detail: []string{long + "A", long + "B", long + "C", long + "D", long + "E", long + "F"},
	}
	for _, size := range []responsive.Size{{Columns: 40, Rows: 10}, {Columns: 48, Rows: 30}, {Columns: 110, Rows: 24}} {
		for _, design := range liveDesigns {
			frame, err := renderSample(Spec{
				Design: design, ThemeID: "catppuccin",
				Viewport: Viewport{ID: "live", Name: "Live", Width: size.Columns, Height: size.Rows},
			}, data, treatments[0])
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%s/%dx%d", design.ID, size.Columns, size.Rows)
			if got := semanticFrameDigest(frame); got != want[key] {
				t.Errorf("%s long-content semantic frame = %s", key, got)
			}
		}
	}
}

func semanticFrameDigest(frame *view.Frame) string {
	digest := sha256.New()
	fmt.Fprintf(digest, "%dx%d\x00", frame.Width(), frame.Height())
	for row := 0; row < frame.Height(); row++ {
		for column := 0; column < frame.Width(); column++ {
			cell, _ := frame.CellAt(column, row)
			fmt.Fprintf(digest, "%q\x00%s\x00%s\x00%d\x00%t\x00%t\x00%t\x00%t\x00",
				cell.Text, cell.Style.Foreground, cell.Style.Background, cell.Width,
				cell.Continuation, cell.Style.Bold, cell.Style.Dim, cell.Style.Underline)
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func TestLiveHUDBoundaryRequiresAtLeastOneHundredColumnsAndNineteenRows(t *testing.T) {
	for _, test := range []struct {
		size         responsive.Size
		want, reject string
	}{
		{size: responsive.Size{Columns: 99, Rows: 19}, want: "99x19>99x19 standard g7 set:1 F1", reject: "HUD 99x19"},
		{size: responsive.Size{Columns: 100, Rows: 18}, want: "100x18>100x18 standard g7 set:1 F1", reject: "HUD 100x18"},
		{size: responsive.Size{Columns: 100, Rows: 19}, want: "HUD 100x19→100x19 standard gen:7 settled:true · fixture:Live terminal", reject: "100x19>100x19"},
	} {
		t.Run(fmt.Sprintf("%dx%d", test.size.Columns, test.size.Rows), func(t *testing.T) {
			surface := NewLiveSurface()
			plain := renderLiveText(t, surface, test.size, true)
			if !strings.Contains(plain, test.want) || strings.Contains(plain, test.reject) {
				t.Fatalf("HUD boundary render want %q and reject %q:\n%s", test.want, test.reject, plain)
			}
		})
	}
}

func TestLiveFixtureLabelsDistinguishLiveAndSizedFixtures(t *testing.T) {
	surface := NewLiveSurface()
	live := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 24}, false)
	if !strings.Contains(live, "Live terminal") || strings.Contains(live, "Live terminal 0x0") {
		t.Fatalf("zero-sized live fixture label is wrong:\n%s", live)
	}

	for index, fixture := range viewportFixtures {
		if fixture.ID == "minimum" {
			surface.fixture = index
			break
		}
	}
	sized := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 24}, false)
	if !strings.Contains(sized, "Minimum Compact 40x10") {
		t.Fatalf("sized fixture label omits its dimensions:\n%s", sized)
	}
}

func TestLiveFlowStatusYieldsToHUDStatus(t *testing.T) {
	surface := NewLiveSurface()
	for index, state := range surface.currentPlugin().States {
		if state.ID == "loading" {
			surface.state = index
			break
		}
	}
	plain := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 24}, false)
	if !strings.Contains(plain, "Working · progress 2/3 · Loading · "+theme.IDs()[0]+" · Live terminal") {
		t.Fatalf("plain status does not preserve flow status:\n%s", plain)
	}

	hud := renderLiveText(t, surface, responsive.Size{Columns: 110, Rows: 24}, true)
	if !strings.Contains(hud, "HUD 110x24→110x24 wide") || strings.Contains(hud, "Working · progress 2/3") {
		t.Fatalf("HUD did not replace flow status:\n%s", hud)
	}
}

func TestLiveHasErrorExactlyForErrorStates(t *testing.T) {
	for pluginIndex, plugin := range pluginSurfaces {
		for stateIndex, state := range plugin.States {
			surface := NewLiveSurface()
			surface.plugin = pluginIndex
			surface.state = stateIndex
			want := state.ID == "error" || state.ID == "validation-error"
			if got := surface.DiagnosticState().HasError; got != want {
				t.Errorf("%s/%s HasError = %t, want %t", plugin.ID, state.ID, got, want)
			}
		}
	}
}

func focusPluginAxis(surface *LiveSurface) {
	for surface.DiagnosticState().Focus.String() != "plugin" {
		surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyTab})
	}
}

func renderLiveText(t *testing.T, surface *LiveSurface, size responsive.Size, hud bool) string {
	t.Helper()
	surface.hud = hud
	layout := responsive.Resolve(size)
	frame, err := surface.Render(shell.RenderContext{Layout: layout, ResizeGeneration: 7, Settled: true})
	if err != nil {
		t.Fatalf("render %dx%d: %v", size.Columns, size.Rows, err)
	}
	return stripSGR(view.ANSI(frame))
}
