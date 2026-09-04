package interaction

import (
	"context"
	"errors"
	"fmt"

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
	Title    string
	Load     Loader
	Activate Activator
}

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
	cursor Cursor
	page   Page
	append bool
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
	loadGeneration       uint64
	activationGeneration uint64
	hasError             bool
	activated            bool
	loadKey              shell.RequestKey
	activationKey        shell.RequestKey
}

func NewPicker(options PickerOptions) (*Picker, error) {
	if options.Title == "" {
		return nil, errors.New("picker title is empty")
	}
	if options.Load == nil {
		return nil, errors.New("picker loader is nil")
	}
	loadKey, _ := shell.NewRequestKey("interaction.picker.load")
	activationKey, _ := shell.NewRequestKey("interaction.picker.activate")
	return &Picker{options: options, loadKey: loadKey, activationKey: activationKey}, nil
}

// Editing reports whether plain text is currently routed into the query.
func (picker *Picker) Editing() bool { return picker.editing }

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
	case shell.ResultEvent:
		picker.result(event)
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
		return picker.startLoad(eventContext, Cursor{}, false)
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
				return picker.startLoad(eventContext, Cursor{}, false)
			}
		case shell.KeyDelete:
			if picker.query.Delete() {
				return picker.startLoad(eventContext, Cursor{}, false)
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

func (picker *Picker) startLoad(eventContext shell.EventContext, cursor Cursor, appendPage bool) []shell.Effect {
	query := picker.query.Text()
	work := func(ctx context.Context) shell.WorkResult {
		page, err := picker.options.Load(ctx, query, cursor)
		if err != nil {
			return shell.WorkResult{Err: err, Code: diagnostics.OutcomeFailed}
		}
		return shell.WorkResult{Value: loadResult{cursor: cursor, page: page, append: appendPage}, Code: diagnostics.OutcomeApplied}
	}
	effect, err := eventContext.Start(picker.loadKey, work)
	if err != nil {
		picker.hasError = true
		return nil
	}
	picker.loadGeneration++
	picker.pendingLoad, picker.hasError, picker.activated = true, false, false
	if !appendPage {
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
			picker.hasError = true
			return
		}
		result, ok := event.Result.Value.(loadResult)
		if !ok || picker.applyPage(result) != nil {
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
	result.page.Items = rankItems(picker.query.Text(), result.page.Items)
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
	return nil
}

func rankItems(query string, items []Item) []Item {
	if query == "" {
		return append([]Item(nil), items...)
	}
	candidates := make([]Candidate, len(items))
	byKey := make(map[string]Item, len(items))
	for index, item := range items {
		candidates[index] = Candidate{Key: item.Key, Text: item.Label + " " + item.Description}
		byKey[item.Key] = item
	}
	matches := Rank(query, candidates)
	result := make([]Item, 0, len(matches))
	for _, match := range matches {
		result = append(result, byKey[match.Candidate.Key])
	}
	return result
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
	if picker.pendingLoad || len(picker.pages) == 0 {
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
	frame.PutText(1, 0, fitCells(picker.options.Title, frame.Width()-2), header)
	lines := []string{
		"Terminal is too small",
		fmt.Sprintf("Need 40×10 · received %d×%d", reported.Columns, reported.Rows),
		"Resize to continue; your state is preserved.",
	}
	for row, line := range lines {
		if row+2 >= frame.Height() {
			break
		}
		frame.PutText(1, row+2, fitCells(line, frame.Width()-2), view.Style{Foreground: palette.Text, Background: palette.Background})
	}
}

func (picker *Picker) renderRoot(frame *view.Frame, palette theme.Palette, split bool) {
	width, height := frame.Width(), frame.Height()
	header := view.Style{Foreground: palette.Accent, Background: palette.PanelBackground, Bold: true}
	frame.Fill(0, 0, width, 1, header)
	frame.PutText(1, 0, fitCells(picker.options.Title, width-2), header)
	search := "Search: " + picker.query.Text()
	if picker.editing {
		search = "Search: " + picker.queryWithCursor()
	}
	frame.PutText(0, 1, "▌", view.Style{Foreground: palette.Accent, Background: palette.Background, Bold: true})
	frame.PutText(2, 1, fitCells(search, width-3), view.Style{Foreground: palette.Text, Background: palette.Background})
	frame.PutText(1, 2, fitCells(picker.status(), width-2), view.Style{Foreground: palette.Muted, Background: palette.Background})
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
	frame.PutText(0, height-1, fitCells(footer, width), view.Style{Foreground: palette.Muted, Background: palette.PanelBackground})
}

func (picker *Picker) renderCompactDetail(frame *view.Frame, palette theme.Palette) {
	width, height := frame.Width(), frame.Height()
	header := view.Style{Foreground: palette.Accent, Background: palette.PanelBackground, Bold: true}
	frame.Fill(0, 0, width, 1, header)
	frame.PutText(1, 0, fitCells("Detail · "+picker.options.Title, width-2), header)
	picker.renderDetail(frame, palette, 1, 2, width-2, height-4)
	frame.PutText(0, height-1, fitCells("Enter/a activate · Backspace/Esc back · ? help", width), view.Style{Foreground: palette.Muted, Background: palette.PanelBackground})
}

func (picker *Picker) renderDetail(frame *view.Frame, palette theme.Palette, x, y, width, height int) {
	item, ok := picker.selectedItem()
	if !ok || width <= 0 || height <= 0 {
		return
	}
	frame.PutText(x, y, fitCells(item.Label, width), view.Style{Foreground: palette.Text, Background: palette.PanelBackground, Bold: true})
	lines := append([]string{item.Description}, item.Detail...)
	for row, line := range lines {
		if row+1 >= height {
			break
		}
		frame.PutText(x, y+row+1, fitCells(line, width), view.Style{Foreground: palette.Muted, Background: palette.PanelBackground})
	}
}

func (picker *Picker) renderHelp(frame *view.Frame, palette theme.Palette) {
	width := frame.Width()
	lines := []string{"PICKER HELP", "/ focus search · arrows edit/move", "Home/End first/last · PageUp/PageDown page", "Enter open/activate · Tab next region", "Backspace edit/back · Escape back/close", "Mobile: j/k move · n/p page · o open", "? close help · q/Ctrl-C quit"}
	for row, line := range lines {
		if row >= frame.Height() {
			break
		}
		style := view.Style{Foreground: palette.Text, Background: palette.Background}
		if row == 0 {
			style = view.Style{Foreground: palette.Accent, Background: palette.PanelBackground, Bold: true}
			frame.Fill(0, 0, width, 1, style)
		}
		frame.PutText(1, row, fitCells(line, width-2), style)
	}
}

func (picker *Picker) status() string {
	state := "Ready"
	switch {
	case picker.pendingActivation:
		state = "Working · activating"
	case picker.pendingLoad:
		state = "Loading…"
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
		Pending: picker.pendingLoad || picker.pendingActivation, HasError: picker.hasError,
	}
}

func (picker *Picker) hasNoResults() bool {
	return picker.loaded && len(picker.items()) == 0
}

func semanticID(value string) diagnostics.ID { id, _ := diagnostics.NewID(value); return id }
