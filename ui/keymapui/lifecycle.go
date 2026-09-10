package keymapui

import (
	"context"
	"errors"
	"reflect"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/documentstore"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
)

type ioResult struct {
	disk diskState
	save bool
}

func (s *Surface) pollLater(events shell.EventContext) []shell.Effect {
	if s.options.DefaultKeymap {
		return nil
	}
	effect, err := events.After(pollInterval, pollCode)
	if err != nil {
		return nil
	}
	return []shell.Effect{effect}
}
func (s *Surface) load(events shell.EventContext) []shell.Effect {
	if s.saving {
		return s.pollLater(events)
	}
	configuration := s.configuration
	effect, err := events.Start(ioKey, func(ctx context.Context) shell.WorkResult {
		return shell.WorkResult{Value: ioResult{disk: configuration.read(ctx)}, Code: diagnostics.OutcomeApplied}
	})
	if err != nil {
		s.warning = "Cannot reload keyboard shortcuts."
		return s.pollLater(events)
	}
	s.loading = true
	return []shell.Effect{effect}
}
func (s *Surface) save(events shell.EventContext) []shell.Effect {
	if !s.draftOpen || s.saving {
		return nil
	}
	if s.disk.revision != s.draftRevision {
		s.status = "File changed; reload or discard."
		return nil
	}
	if err := s.runtime.Validate(s.draft); err != nil {
		s.status = err.Error()
		return nil
	}
	draft, revision, configuration := keymap.Clone(s.draft), s.draftRevision, s.configuration
	effect, err := events.Start(ioKey, func(ctx context.Context) shell.WorkResult {
		disk, err := configuration.save(ctx, draft, revision)
		code := diagnostics.OutcomeApplied
		if err != nil {
			code = diagnostics.OutcomeFailed
		}
		return shell.WorkResult{Value: ioResult{disk: disk, save: true}, Code: code, Err: err}
	})
	if err != nil {
		s.status = "Cannot save keyboard shortcuts."
		return nil
	}
	s.saving = true
	s.loading = false
	s.status = "Saving keyboard shortcuts…"
	return []shell.Effect{effect}
}
func (s *Surface) finishIO(events shell.EventContext, event shell.ResultEvent) []shell.Effect {
	value, ok := event.Result.Value.(ioResult)
	if !ok {
		return s.pollLater(events)
	}
	s.loading = false
	if value.save {
		s.saving = false
		if event.Result.Err != nil {
			if errors.Is(event.Result.Err, documentstore.ErrConflict) {
				s.status = "File changed; reload or discard."
			} else {
				s.status = "Cannot save keyboard shortcuts; draft kept."
			}
			return s.load(events)
		}
		s.disk = value.disk
		s.latestValid = keymap.Clone(s.disk.document)
		s.warning = ""
		s.draftRevision = s.disk.revision
		s.draftOpen = false
		s.draft = keymap.Document{}
		s.captured = nil
		s.text.Set("")
		s.actionsFocused = false
		s.view = s.editorOrigin
		s.applyLatest()
		s.status = "Keyboard shortcuts saved."
		return append(s.resizeActive(events), s.pollLater(events)...)
	}
	changed := value.disk.revision != s.disk.revision || value.disk.warning != s.disk.warning
	s.disk = value.disk
	s.warning = s.disk.warning
	if s.disk.valid {
		s.latestValid = keymap.Clone(s.disk.document)
	}
	if s.reloadRequested {
		s.reloadRequested = false
		s.applyLatest()
		s.resetDraft()
		s.status = "Saved shortcuts reloaded into the draft."
		if s.draftOpen {
			if s.view == entryScreen {
				return append(s.refreshEntry(events), s.pollLater(events)...)
			}
			return append(s.refreshEditor(events), s.pollLater(events)...)
		}
	} else if !s.draftOpen && s.view != checkerScreen {
		s.applyLatest()
	}
	if changed && s.draftOpen && s.disk.revision != s.draftRevision {
		s.status = "File changed; reload or discard."
		if s.view == editorScreen {
			return append(s.refreshEditor(events), s.pollLater(events)...)
		}
	}
	return s.pollLater(events)
}

func (s *Surface) applyLatest() {
	if s.options.DefaultKeymap {
		return
	}
	// Reapplying an unchanged document would cancel an in-progress sequence.
	if reflect.DeepEqual(s.runtime.Document(), s.latestValid) {
		return
	}
	_ = s.runtime.Apply(s.latestValid)
}
