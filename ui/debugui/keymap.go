package debugui

import (
	"fmt"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
)

func (surface *Surface) contextName() string {
	if surface.editing {
		return "debug.editing"
	}
	return []string{"debug.health", "debug.timeline", "debug.gallery", "debug.hud", "debug.detail", "debug.help"}[surface.screen]
}

func (surface *Surface) KeymapCatalog() []keymap.Action {
	var out []keymap.Action
	add := func(context, id, label string, quick bool, keys ...string) {
		d := make([]keymap.Sequence, len(keys))
		for i, k := range keys {
			d[i] = keymap.Sequence{k}
		}
		out = append(out, keymap.Action{ID: id, Label: label, Context: context, Quick: quick, Defaults: d})
	}
	for _, ctx := range []string{"debug.health", "debug.timeline", "debug.gallery", "debug.hud", "debug.detail", "debug.help"} {
		add(ctx, "debug.quit", "Quit", true, "q")
		add(ctx, "debug.back", "Back", true, "escape", "backspace")
		add(ctx, "debug.freeze", "Freeze or resume", true, "space", "p")
		add(ctx, "debug.hide-debugger", "Show or hide debugger events", true, "v")
		add(ctx, "debug.help", "Help", true, "?")
		add(ctx, "debug.health", "Health", true, "h")
		add(ctx, "debug.timeline", "Timeline", true, "t")
		add(ctx, "debug.gallery", "Gallery", true, "g")
		add(ctx, "debug.hud", "HUD", true, "r")
		add(ctx, "debug.export", "Export", true, "e")
		add(ctx, "debug.next-session", "Next session", true, "s")
		add(ctx, "debug.level", "Cycle level", true, "l")
		add(ctx, "debug.kind", "Cycle kind", true, "k")
		add(ctx, "debug.clear-filters", "Clear filters", true, "0")
		add(ctx, "debug.filter-component", "Filter component", true, "1")
		add(ctx, "debug.filter-action", "Filter action", true, "2")
		add(ctx, "debug.filter-code", "Filter code", true, "3")
		add(ctx, "debug.filter-correlation", "Filter correlation", true, "4", "c")
		add(ctx, "debug.edit-filter", "Edit code filter", true, "/")
		add(ctx, "debug.previous", "Previous event", false, "up")
		add(ctx, "debug.next", "Next event", false, "down")
		add(ctx, "debug.first", "First event", false, "home")
		add(ctx, "debug.last", "Last event", false, "end")
		add(ctx, "debug.previous-page", "Previous page", false, "page-up")
		add(ctx, "debug.next-page", "Next page", false, "page-down")
		add(ctx, "debug.previous-session", "Previous session", false, "left")
		add(ctx, "debug.next-session-nav", "Next session", false, "right")
		add(ctx, "debug.open", "Open detail", true, "enter")
	}
	ed := func(id, label string, keys ...string) {
		d := make([]keymap.Sequence, len(keys))
		for i, k := range keys {
			d[i] = keymap.Sequence{k}
		}
		out = append(out, keymap.Action{ID: id, Label: label, Context: "debug.editing", Editing: true, Defaults: d})
	}
	ed("debug.edit-cancel", "Cancel filter edit", "escape")
	ed("debug.edit-backspace", "Delete before cursor", "backspace")
	ed("debug.edit-apply", "Apply filter", "enter")
	return out
}

func (surface *Surface) KeymapContext() keymap.Context {
	target := ""
	if event := surface.selectedEvent(); event != nil {
		target = fmt.Sprintf("%s:%d:%d", event.Session.String(), event.Sequence, surface.projectionEpoch)
	} else {
		target = fmt.Sprintf("none:%d", surface.projectionEpoch)
	}
	return keymap.Context{ID: surface.contextName(), Editing: surface.editing, Target: target}
}

func (surface *Surface) ActionState(id string) keymap.State {
	known := false
	ctx := surface.contextName()
	for _, a := range surface.KeymapCatalog() {
		if a.Context == ctx && a.ID == id {
			known = true
			break
		}
	}
	if !known {
		return keymap.State{Reason: "unavailable in current view"}
	}
	switch id {
	case "debug.export":
		return keymap.State{Enabled: surface.options.Exporter != nil && surface.export != exportPending && surface.view != nil, Reason: "export unavailable"}
	case "debug.previous-page":
		return keymap.State{Enabled: surface.screen == screenDetail || surface.page.HasPrev, Reason: "no previous page"}
	case "debug.next-page":
		return keymap.State{Enabled: surface.screen == screenDetail || surface.page.HasNext, Reason: "no next page"}
	case "debug.open", "debug.filter-component", "debug.filter-action", "debug.filter-code", "debug.filter-correlation":
		enabled := surface.selectedEvent() != nil
		if id == "debug.open" {
			enabled = enabled && (surface.screen == screenTimeline || surface.screen == screenGallery)
		}
		return keymap.State{Enabled: enabled, Reason: "no selected event"}
	}
	return keymap.State{Enabled: true}
}

func (surface *Surface) action(events shell.EventContext, id string) []shell.Effect {
	if !surface.ActionState(id).Enabled {
		return nil
	}
	keys := map[string]shell.KeyCode{"debug.back": shell.KeyEscape, "debug.freeze": shell.KeySpace, "debug.previous": shell.KeyUp, "debug.next": shell.KeyDown, "debug.first": shell.KeyHome, "debug.last": shell.KeyEnd, "debug.previous-page": shell.KeyPageUp, "debug.next-page": shell.KeyPageDown, "debug.previous-session": shell.KeyLeft, "debug.next-session-nav": shell.KeyRight, "debug.open": shell.KeyEnter, "debug.edit-cancel": shell.KeyEscape, "debug.edit-backspace": shell.KeyBackspace, "debug.edit-apply": shell.KeyEnter}
	if key, ok := keys[id]; ok {
		return surface.key(events, key)
	}
	chars := map[string]byte{"debug.quit": 'q', "debug.hide-debugger": 'v', "debug.help": '?', "debug.health": 'h', "debug.timeline": 't', "debug.gallery": 'g', "debug.hud": 'r', "debug.export": 'e', "debug.next-session": 's', "debug.level": 'l', "debug.kind": 'k', "debug.clear-filters": '0', "debug.filter-component": '1', "debug.filter-action": '2', "debug.filter-code": '3', "debug.filter-correlation": '4', "debug.edit-filter": '/'}
	if ch, ok := chars[id]; ok {
		return surface.text(events, ch)
	}
	return nil
}
