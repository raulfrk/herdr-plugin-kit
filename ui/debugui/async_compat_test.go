package debugui

import (
	"context"
	"errors"
	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
)

// These helpers let package-internal state-machine tests use synchronous
// projections; production acquisition is exercised through shell effects.
func (surface *Surface) refresh() {
	snapshot, err := surface.options.Recorder.DebugSnapshot(context.Background(), surface.options.Previews)
	if err != nil {
		surface.projectionFailed = true
		return
	}
	view, err := snapshot.Select(context.Background(), surface.query, surface.list == screenGallery)
	if err != nil {
		surface.projectionFailed = true
		return
	}
	if !surface.query.Session.IsZero() {
		found := false
		for _, session := range view.Sessions() {
			if session.ID == surface.query.Session {
				found = true
				break
			}
		}
		if !found {
			surface.query.Session = diagnostics.ID{}
			surface.session = 0
			surface.page = diagnostics.DebugPage{Sessions: view.Sessions(), Health: view.Health(), PageSize: surface.query.PageSize}
			surface.projectionFailed = false
			return
		}
	}
	surface.snapshot, surface.view, surface.loaded = snapshot, view, true
	surface.displayedQuery, surface.displayedList = surface.query, surface.list
	surface.projectionFailed = false
	surface.rebuildWindow(false)
}

func (surface *Surface) exportWork() func(context.Context) shell.WorkResult {
	view, window, limit, previews, exporter := surface.view, surface.window, surface.options.MaxReportBytes, surface.options.Previews, surface.options.Exporter
	return func(ctx context.Context) shell.WorkResult {
		if view == nil {
			surface.refresh()
			view, window = surface.view, surface.window
		}
		data, err := view.ExportWindow(ctx, window, limit, previews)
		if err == nil && exporter != nil {
			err = exporter(ctx, data)
		}
		if err != nil {
			return shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: errors.New("debug export failed")}
		}
		return shell.WorkResult{Code: diagnostics.OutcomeApplied}
	}
}
