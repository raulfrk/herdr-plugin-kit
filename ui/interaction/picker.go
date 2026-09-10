package interaction

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

// Cursor is an uninterpreted provider paging token. Its empty value means the
// first page when loading and no next page when returned by a provider.
type Cursor struct{ token string }

func NewCursor(token string) Cursor { return Cursor{token: token} }
func (cursor Cursor) Token() string { return cursor.token }
func (cursor Cursor) Empty() bool   { return cursor.token == "" }

type Item struct {
	Key         string
	Label       string
	Description string
	Detail      []string
	Disabled    bool
	Value       any
}

type Page struct {
	Items []Item
	Next  Cursor
}

type Loader func(context.Context, string, Cursor) (Page, error)
type Activator func(context.Context, Item) error

type PickerOptions struct {
	Title     string
	Namespace string
	Load      Loader
	Activate  Activator
}

const queryDebounce time.Duration = 80_000_000

type pickerScreen uint8

const (
	rootScreen pickerScreen = iota
	detailScreen
	helpScreen
)

type loadedPage struct {
	cursor Cursor
	page   Page
}

type loadResult struct {
	cursor   Cursor
	page     Page
	append   bool
	query    string
	revision uint64
}

type activationResult struct{}

// Picker is reusable state and a shell.Surface. Provider data and query text
// remain in memory and are excluded from its semantic diagnostic projection.
type Picker struct {
	options          PickerOptions
	query            Buffer
	layout           responsive.Layout
	resizeGeneration uint64

	pages                []loadedPage
	pageIndex            int
	selected             int
	selectedKey          string
	screen               pickerScreen
	returnScreen         pickerScreen
	editing              bool
	loaded               bool
	pendingLoad          bool
	pendingActivation    bool
	queryDirty           bool
	queryRevision        uint64
	targetRevision       uint64
	loadGeneration       uint64
	activationGeneration uint64
	hasError             bool
	activated            bool
	loadKey              shell.RequestKey
	activationKey        shell.RequestKey
	queryDebounceCode    shell.EventCode
}

func NewPicker(options PickerOptions) (*Picker, error) {
	if options.Title == "" {
		return nil, errors.New("picker title is empty")
	}
	if options.Load == nil {
		return nil, errors.New("picker loader is nil")
	}
	namespace := options.Namespace
	if namespace == "" {
		namespace = "interaction.picker"
	} else if _, err := diagnostics.NewID(namespace); err != nil {
		return nil, fmt.Errorf("invalid picker namespace: %w", err)
	}
	loadKey, err := shell.NewRequestKey(namespace + ".load")
	if err != nil {
		return nil, fmt.Errorf("invalid picker load key: %w", err)
	}
	activationKey, err := shell.NewRequestKey(namespace + ".activate")
	if err != nil {
		return nil, fmt.Errorf("invalid picker activation key: %w", err)
	}
	queryDebounceCode, err := shell.NewEventCode(namespace + ".query")
	if err != nil {
		return nil, fmt.Errorf("invalid picker query timer: %w", err)
	}
	for _, suffix := range []string{".results", ".editing", ".detail", ".help"} {
		if _, err := diagnostics.NewID(namespace + suffix); err != nil {
			return nil, fmt.Errorf("invalid picker context: %w", err)
		}
	}
	return &Picker{options: options, loadKey: loadKey, activationKey: activationKey, queryDebounceCode: queryDebounceCode}, nil
}

// Editing reports whether plain text is currently routed into the query.
func (picker *Picker) Editing() bool { return picker.editing }

// Selected returns the current enabled item. The false result covers empty
// pages, invalid selection state, and disabled items.
func (picker *Picker) Selected() (Item, bool) { return picker.selectedItem() }

// Refresh reloads the first page while preserving the query and selected item identity.
func (picker *Picker) Refresh(events shell.EventContext) []shell.Effect {
	return picker.startLoad(events, Cursor{}, false)
}

func (picker *Picker) Update(eventContext shell.EventContext, event shell.Event) []shell.Effect {
	switch event := event.(type) {
	case shell.ResizeEvent:
		if event.Generation <= picker.resizeGeneration {
			return nil
		}
		picker.layout, picker.resizeGeneration = event.Layout, event.Generation
		if picker.loaded {
			break
		}
		if picker.pendingLoad {
			break
		}
		return picker.startLoad(eventContext, Cursor{}, false)
	case shell.TextEvent:
		return picker.text(eventContext, event.Text)
	case shell.KeyEvent:
		return picker.key(eventContext, event.Code)
	case shell.ActionEvent:
		return picker.action(eventContext, event.ID)
	case shell.ResultEvent:
		picker.result(event)
	case shell.TimerEvent:
		if event.Code.String() == picker.queryDebounceCode.String() {
			return picker.startLoad(eventContext, Cursor{}, false)
		}
	}
	return nil
}

func (picker *Picker) text(eventContext shell.EventContext, text string) []shell.Effect {
	if picker.screen == helpScreen {
		if text == "?" {
			picker.screen = picker.returnScreen
		} else if text == "q" {
			return []shell.Effect{shell.Quit()}
		}
		return nil
	}
	if picker.editing {
		picker.query.Insert(text)
		return picker.scheduleQueryLoad(eventContext)
	}
	if picker.screen == detailScreen {
		if text == "a" || text == "o" {
			return picker.activate(eventContext)
		}
		if text == "?" {
			picker.returnScreen, picker.screen = detailScreen, helpScreen
		}
		return nil
	}
	switch text {
	case "/":
		picker.editing = true
	case "?":
		picker.returnScreen, picker.screen = picker.screen, helpScreen
	case "q":
		return []shell.Effect{shell.Quit()}
	case "j":
		picker.move(1)
	case "k":
		picker.move(-1)
	case "n":
		return picker.nextPage(eventContext)
	case "p":
		picker.previousPage()
	case "o":
		return picker.openOrActivate(eventContext)
	}
	return nil
}

func (picker *Picker) key(eventContext shell.EventContext, key shell.KeyCode) []shell.Effect {
	if key == shell.KeyCtrlC {
		return []shell.Effect{shell.Quit()}
	}
	if picker.screen == helpScreen {
		switch key {
		case shell.KeyEscape, shell.KeyBackspace, shell.KeyEnter:
			picker.screen = picker.returnScreen
		}
		return nil
	}
	if picker.editing {
		switch key {
		case shell.KeyEscape, shell.KeyEnter:
			picker.editing = false
		case shell.KeyBackspace:
			if picker.query.Backspace() {
				return picker.scheduleQueryLoad(eventContext)
			}
		case shell.KeyDelete:
			if picker.query.Delete() {
				return picker.scheduleQueryLoad(eventContext)
			}
		case shell.KeyLeft:
			picker.query.MoveLeft()
		case shell.KeyRight:
			picker.query.MoveRight()
		case shell.KeyHome:
			picker.query.MoveHome()
		case shell.KeyEnd:
			picker.query.MoveEnd()
		case shell.KeyTab:
			picker.editing = false
		}
		return nil
	}
	if picker.screen == detailScreen {
		switch key {
		case shell.KeyEscape, shell.KeyBackspace:
			picker.screen = rootScreen
		case shell.KeyEnter:
			return picker.activate(eventContext)
		}
		return nil
	}
	switch key {
	case shell.KeyEscape:
		return []shell.Effect{shell.Quit()}
	case shell.KeyBackspace:
		return []shell.Effect{shell.Quit()}
	case shell.KeyTab:
		picker.editing = true
	case shell.KeyUp:
		picker.move(-1)
	case shell.KeyDown:
		picker.move(1)
	case shell.KeyHome:
		picker.selectBoundary(false)
	case shell.KeyEnd:
		picker.selectBoundary(true)
	case shell.KeyPageUp:
		picker.previousPage()
	case shell.KeyPageDown:
		return picker.nextPage(eventContext)
	case shell.KeyEnter:
		return picker.openOrActivate(eventContext)
	}
	return nil
}

func (picker *Picker) scheduleQueryLoad(eventContext shell.EventContext) []shell.Effect {
	// A started load increments loadGeneration, so its current value is a
	// distinct revision from every request that can still be in flight.
	picker.queryRevision = picker.loadGeneration
	picker.queryDirty = true
	effect, err := eventContext.After(queryDebounce, picker.queryDebounceCode)
	if err != nil {
		picker.queryDirty = false
		picker.hasError = true
		return nil
	}
	picker.hasError = false
	return []shell.Effect{effect}
}

func (picker *Picker) startLoad(eventContext shell.EventContext, cursor Cursor, appendPage bool) []shell.Effect {
	query := picker.query.Text()
	revision := picker.queryRevision
	work := func(ctx context.Context) shell.WorkResult {
		page, err := picker.options.Load(ctx, query, cursor)
		if err != nil {
			return shell.WorkResult{Value: loadResult{query: query, revision: revision}, Err: err, Code: diagnostics.OutcomeFailed}
		}
		return shell.WorkResult{Value: loadResult{cursor: cursor, page: page, append: appendPage, query: query, revision: revision}, Code: diagnostics.OutcomeApplied}
	}
	effect, err := eventContext.Start(picker.loadKey, work)
	if err != nil {
		if !appendPage {
			picker.queryDirty = false
		}
		picker.hasError = true
		return nil
	}
	picker.loadGeneration++
	picker.pendingLoad, picker.hasError, picker.activated = true, false, false
	if !appendPage {
		picker.queryDirty = false
		picker.pages, picker.pageIndex = nil, 0
	}
	return []shell.Effect{effect}
}

func (picker *Picker) result(event shell.ResultEvent) {
	switch event.Key.String() {
	case picker.loadKey.String():
		if event.Generation != picker.loadGeneration {
			return
		}
		picker.pendingLoad = false
		if event.Result.Err != nil {
			result, ok := event.Result.Value.(loadResult)
			if ok {
				if result.query != picker.query.Text() {
					return
				}
				if result.revision != picker.queryRevision {
					return
				}
			}
			picker.hasError = true
			return
		}
		result, ok := event.Result.Value.(loadResult)
		if !ok {
			picker.hasError = true
			return
		}
		if result.query != picker.query.Text() || result.revision != picker.queryRevision {
			return
		}
		if picker.applyPage(result) != nil {
			picker.hasError = true
		}
	case picker.activationKey.String():
		if event.Generation != picker.activationGeneration {
			return
		}
		picker.pendingActivation = false
		picker.hasError = event.Result.Err != nil
		picker.activated = event.Result.Err == nil
	}
}

func (picker *Picker) applyPage(result loadResult) error {
	seen := make(map[string]struct{}, len(result.page.Items))
	for _, item := range result.page.Items {
		if item.Key == "" {
			return errors.New("picker item key is empty")
		}
		if _, exists := seen[item.Key]; exists {
			return fmt.Errorf("duplicate picker item key %q", item.Key)
		}
		seen[item.Key] = struct{}{}
	}
	entry := loadedPage{cursor: result.cursor, page: result.page}
	if result.append {
		picker.pages = append(picker.pages, entry)
		picker.pageIndex = len(picker.pages) - 1
	} else {
		picker.pages = []loadedPage{entry}
		picker.pageIndex = 0
	}
	picker.loaded, picker.hasError = true, false
	picker.restoreSelection()
	picker.targetRevision++
	return nil
}

func (picker *Picker) restoreSelection() {
	items := picker.items()
	for index, item := range items {
		if item.Key == picker.selectedKey && !item.Disabled {
			picker.selected = index
			return
		}
	}
	picker.selected = firstEnabled(items)
	picker.rememberSelection()
}

func firstEnabled(items []Item) int {
	for index, item := range items {
		if !item.Disabled {
			return index
		}
	}
	return 0
}

func (picker *Picker) move(delta int) {
	items := picker.items()
	if len(items) == 0 {
		return
	}
	for index := picker.selected + delta; index >= 0 && index < len(items); index += delta {
		if !items[index].Disabled {
			picker.selected = index
			picker.rememberSelection()
			return
		}
	}
}

func (picker *Picker) selectBoundary(end bool) {
	items := picker.items()
	start, step := 0, 1
	if end {
		start, step = len(items)-1, -1
	}
	for index := start; index >= 0 && index < len(items); index += step {
		if !items[index].Disabled {
			picker.selected = index
			picker.rememberSelection()
			return
		}
	}
}

func (picker *Picker) rememberSelection() {
	if item, ok := picker.selectedItem(); ok {
		picker.selectedKey = item.Key
	}
}

func (picker *Picker) previousPage() {
	if picker.pageIndex == 0 || len(picker.pages) == 0 {
		return
	}
	picker.pageIndex--
	picker.restoreSelection()
}

func (picker *Picker) nextPage(eventContext shell.EventContext) []shell.Effect {
	if picker.queryDirty {
		return nil
	}
	if picker.pendingLoad {
		return nil
	}
	if len(picker.pages) == 0 {
		return nil
	}
	if picker.pageIndex+1 < len(picker.pages) {
		picker.pageIndex++
		picker.restoreSelection()
		return nil
	}
	next := picker.pages[picker.pageIndex].page.Next
	if next.Empty() {
		return nil
	}
	return picker.startLoad(eventContext, next, true)
}

func (picker *Picker) openOrActivate(eventContext shell.EventContext) []shell.Effect {
	if picker.queryDirty {
		return nil
	}
	if _, ok := picker.selectedItem(); !ok {
		return nil
	}
	if compactPresentation(picker.layout) {
		picker.screen = detailScreen
		return nil
	}
	return picker.activate(eventContext)
}

func (picker *Picker) activate(eventContext shell.EventContext) []shell.Effect {
	if picker.queryDirty {
		return nil
	}
	item, ok := picker.selectedItem()
	if !ok {
		return nil
	}
	if item.Disabled {
		return nil
	}
	if picker.options.Activate == nil {
		return nil
	}
	if picker.pendingActivation {
		return nil
	}
	work := func(ctx context.Context) shell.WorkResult {
		if err := picker.options.Activate(ctx, item); err != nil {
			return shell.WorkResult{Err: err, Code: diagnostics.OutcomeFailed}
		}
		return shell.WorkResult{Value: activationResult{}, Code: diagnostics.OutcomeApplied}
	}
	effect, err := eventContext.Start(picker.activationKey, work)
	if err != nil {
		picker.hasError = true
		return nil
	}
	picker.activationGeneration++
	picker.pendingActivation, picker.hasError = true, false
	return []shell.Effect{effect}
}

func (picker *Picker) items() []Item {
	if len(picker.pages) == 0 || picker.pageIndex >= len(picker.pages) {
		return nil
	}
	return picker.pages[picker.pageIndex].page.Items
}

func (picker *Picker) selectedItem() (Item, bool) {
	items := picker.items()
	if picker.selected < 0 || picker.selected >= len(items) || items[picker.selected].Disabled {
		return Item{}, false
	}
	return items[picker.selected], true
}

func compactPresentation(layout responsive.Layout) bool {
	return layout.Render.Columns < 80 || layout.Render.Rows <= 18
}

func (picker *Picker) Render(context shell.RenderContext) (*view.Frame, error) {
	size, palette := context.Layout.Render, context.Theme
	frame, err := view.NewFrame(size.Columns, size.Rows)
	if err != nil {
		return nil, err
	}
	base := view.Style{Foreground: palette.Text, Background: palette.Background}
	frame.Fill(0, 0, size.Columns, size.Rows, base)
	if size.Columns == 0 || size.Rows == 0 {
		return frame, nil
	}
	if context.Layout.Class == responsive.Recovery {
		picker.renderRecovery(frame, palette, context.Layout.Reported)
		return frame, nil
	}
	if picker.screen == helpScreen {
		picker.renderHelp(frame, palette)
		return frame, nil
	}
	if picker.screen == detailScreen && compactPresentation(context.Layout) {
		picker.renderCompactDetail(frame, palette)
		return frame, nil
	}
	picker.renderRoot(frame, palette, !compactPresentation(context.Layout))
	return frame, nil
}

func (picker *Picker) renderRecovery(frame *view.Frame, palette theme.Palette, reported responsive.Size) {
	header := view.Style{Foreground: palette.Accent, Background: palette.PanelBackground, Bold: true}
	frame.Fill(0, 0, frame.Width(), 1, header)
	putTextBox(frame, "picker-recovery-title", 0, 1, 0, frame.Width()-2, 1, view.TextTruncate, false, []string{picker.options.Title}, header)
	lines := []string{
		"Terminal is too small",
		fmt.Sprintf("Need 40×10 · received %d×%d", reported.Columns, reported.Rows),
		"Resize to continue; your state is preserved.",
	}
	putTextBox(frame, "picker-recovery-body", 0, 1, 2, frame.Width()-2, frame.Height()-2, view.TextTruncate, false, lines, view.Style{Foreground: palette.Text, Background: palette.Background})
}

func (picker *Picker) renderRoot(frame *view.Frame, palette theme.Palette, split bool) {
	width, height := frame.Width(), frame.Height()
	header := view.Style{Foreground: palette.Accent, Background: palette.PanelBackground, Bold: true}
	frame.Fill(0, 0, width, 1, header)
	putTextBox(frame, "picker-title", 0, 1, 0, width-2, 1, view.TextTruncate, false, []string{picker.options.Title}, header)
	search := "Search: " + picker.query.Text()
	if picker.editing {
		search = "Search: " + picker.queryWithCursor()
	}
	frame.PutText(0, 1, "▌", view.Style{Foreground: palette.Accent, Background: palette.Background, Bold: true})
	putTextBox(frame, "picker-search", 0, 2, 1, width-3, 1, view.TextTruncate, false, []string{search}, view.Style{Foreground: palette.Text, Background: palette.Background})
	putTextBox(frame, "picker-status", 0, 1, 2, width-2, 1, view.TextTruncate, false, []string{picker.status()}, view.Style{Foreground: palette.Muted, Background: palette.Background})
	listWidth := width
	if split {
		listWidth = width * 2 / 3
		frame.Fill(listWidth, 1, width-listWidth, max(0, height-2), view.Style{Background: palette.PanelBackground})
		picker.renderDetail(frame, palette, listWidth+1, 2, width-listWidth-2, height-4)
	}
	rows := max(0, height-4)
	items := picker.items()
	start := 0
	if rows > 0 && picker.selected >= rows {
		start = picker.selected - rows + 1
	}
	for row := 0; row < rows && start+row < len(items); row++ {
		itemIndex := start + row
		item := items[itemIndex]
		style := view.Style{Foreground: palette.Text, Background: palette.Background}
		prefix := "  "
		if item.Disabled {
			prefix, style.Foreground, style.Dim = "× ", palette.Muted, true
		} else if itemIndex == picker.selected {
			prefix, style.Background, style.Bold = "▌ ", palette.ActiveRowBackground, true
			frame.Fill(0, row+3, listWidth, 1, style)
		}
		frame.PutText(0, row+3, fitCells(prefix+item.Label, listWidth), style)
	}
	footer := "? help · / search · j/k move · o open · n/p page · q quit"
	putTextBox(frame, "picker-footer", 0, 0, height-1, width, 1, view.TextTruncate, false, []string{footer}, view.Style{Foreground: palette.Muted, Background: palette.PanelBackground})
}

func (picker *Picker) renderCompactDetail(frame *view.Frame, palette theme.Palette) {
	width, height := frame.Width(), frame.Height()
	header := view.Style{Foreground: palette.Accent, Background: palette.PanelBackground, Bold: true}
	frame.Fill(0, 0, width, 1, header)
	putTextBox(frame, "picker-detail-header", 0, 1, 0, width-2, 1, view.TextTruncate, false, []string{"Detail · " + picker.options.Title}, header)
	picker.renderDetail(frame, palette, 1, 2, width-2, height-4)
	putTextBox(frame, "picker-detail-footer", 0, 0, height-1, width, 1, view.TextTruncate, false, []string{"Enter/a activate · Backspace/Esc back · ? help"}, view.Style{Foreground: palette.Muted, Background: palette.PanelBackground})
}

func (picker *Picker) renderDetail(frame *view.Frame, palette theme.Palette, x, y, width, height int) {
	item, ok := picker.selectedItem()
	if !ok {
		return
	}
	putTextBox(frame, "picker-detail-title", 0, x, y, width, min(1, max(0, height)), view.TextTruncate, false, []string{item.Label}, view.Style{Foreground: palette.Text, Background: palette.PanelBackground, Bold: true})
	lines := append([]string{item.Description}, item.Detail...)
	putTextBox(frame, "picker-detail-body", 0, x, y+1, width, height-1, view.TextTruncate, false, lines, view.Style{Foreground: palette.Muted, Background: palette.PanelBackground})
}

func (picker *Picker) renderHelp(frame *view.Frame, palette theme.Palette) {
	width := frame.Width()
	lines := []string{"PICKER HELP", "/ focus search · arrows edit/move", "Home/End first/last · PageUp/PageDown page", "Enter open/activate · Tab next region", "Backspace edit/back · Escape back/close", "Mobile: j/k move · n/p page · o open", "? close help · q/Ctrl-C quit"}
	header := view.Style{Foreground: palette.Accent, Background: palette.PanelBackground, Bold: true}
	frame.Fill(0, 0, width, 1, header)
	putTextBox(frame, "picker-help-header", 0, 1, 0, width-2, 1, view.TextTruncate, false, lines[:1], header)
	putTextBox(frame, "picker-help-body", 0, 1, 1, width-2, frame.Height()-1, view.TextTruncate, false, lines[1:], view.Style{Foreground: palette.Text, Background: palette.Background})
}

func putTextBox(frame *view.Frame, element string, instance, x, y, width, height int, mode view.TextMode, allowTruncation bool, lines []string, style view.Style) {
	id, _ := diagnostics.NewID(element)
	_ = frame.PutTextBox(view.TextBoxOptions{
		Element: id, Instance: instance, X: x, Y: y, Width: max(0, width), Height: max(0, height),
		Mode: mode, AllowTruncation: allowTruncation,
	}, lines, style)
}

func (picker *Picker) status() string {
	state := "Ready"
	switch {
	case picker.pendingActivation:
		state = "Working · activating"
	case picker.pendingLoad:
		state = "Loading…"
	case picker.queryDirty:
		state = "Typing…"
	case picker.hasError:
		state = "! Recoverable error"
	case picker.activated:
		state = "OK Activated"
	case picker.hasNoResults():
		state = "0 results · clear search"
	}
	if len(picker.pages) > 0 {
		state += fmt.Sprintf(" · page %d · %d items", picker.pageIndex+1, len(picker.items()))
	}
	return state
}

func (picker *Picker) queryWithCursor() string {
	parts := append([]string(nil), picker.query.graphemes...)
	parts = append(parts, "")
	copy(parts[picker.query.cursor+1:], parts[picker.query.cursor:])
	parts[picker.query.cursor] = "▏"
	result := ""
	for _, part := range parts {
		result += part
	}
	return result
}

func fitCells(text string, width int) string {
	return view.Truncate(text, width, "…")
}

func (picker *Picker) DiagnosticState() diagnostics.VisualState {
	screen := "picker.root"
	focus := "results"
	if picker.screen == detailScreen {
		screen, focus = "picker.detail", "detail"
	} else if picker.screen == helpScreen {
		screen, focus = "picker.help", "help"
	} else if picker.editing {
		focus = "search"
	}
	state := "ready"
	switch {
	case picker.pendingActivation:
		state = "activating"
	case picker.pendingLoad:
		state = "loading"
	case picker.queryDirty:
		state = "debouncing"
	case picker.hasError:
		state = "error"
	case picker.activated:
		state = "activated"
	case picker.hasNoResults():
		state = "empty"
	}
	selection := "none"
	if item, ok := picker.selectedItem(); ok {
		selection = "available"
		if item.Disabled {
			selection = "disabled"
		}
	}
	return diagnostics.VisualState{
		Screen: semanticID(screen), Focus: semanticID(focus), Selection: semanticID(selection), State: semanticID(state),
		Geometry:         diagnostics.Geometry{ReportedColumns: picker.layout.Reported.Columns, ReportedRows: picker.layout.Reported.Rows, RenderColumns: picker.layout.Render.Columns, RenderRows: picker.layout.Render.Rows},
		ResizeGeneration: picker.resizeGeneration, RequestGeneration: picker.loadGeneration,
		ItemCount: len(picker.items()), SelectedIndex: max(0, picker.selected),
		Pending: picker.queryDirty || picker.pendingLoad || picker.pendingActivation, HasError: picker.hasError,
	}
}

func (picker *Picker) hasNoResults() bool {
	return picker.loaded && len(picker.items()) == 0
}

func semanticID(value string) diagnostics.ID { id, _ := diagnostics.NewID(value); return id }
