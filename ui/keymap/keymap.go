// Package keymap declares contextual actions and resolves normalized key sequences.
// Runtime is owned by the UI event loop; it never performs I/O.
package keymap

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
)

type Sequence []string

type Action struct {
	ID, Label, Context string
	Editing, Quick     bool
	Defaults           []Sequence
}

// Context.Target is an opaque identity/revision, never a diagnostic value.
type Context struct {
	ID      string
	Editing bool
	Target  string
}

type State struct {
	Enabled bool
	Reason  string
}

type Binding struct {
	Context   string     `toml:"context"`
	Action    string     `toml:"action"`
	Sequences []Sequence `toml:"sequences"`
}

type Document struct {
	Version  int       `toml:"version"`
	Bindings []Binding `toml:"bindings"`
}

const Timeout = time.Second

// Normalize canonicalizes names delivered by Bubble Tea and names entered in
// the editor. Printable keys retain their case, as terminals encode Shift in
// the resulting character rather than as a separate modifier.
func Normalize(value string) (string, error) {
	if value == " " {
		return "space", nil
	}
	if utf8.RuneCountInString(value) == 1 {
		r, _ := utf8.DecodeRuneInString(value)
		if unicode.IsPrint(r) {
			return value, nil
		}
	}
	if strings.HasSuffix(value, "+ ") {
		value = strings.TrimSuffix(value, " ") + "space"
	}
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "ctrl-c") {
		value = "ctrl+c"
	}
	mods := map[string]bool{}
	base := value
	for {
		part, rest, found := strings.Cut(base, "+")
		part = strings.ToLower(part)
		if !found || (part != "ctrl" && part != "alt" && part != "shift") {
			break
		}
		if mods[part] {
			return "", errors.New("duplicate key modifier")
		}
		mods[part] = true
		base = rest
	}
	if utf8.RuneCountInString(base) != 1 || mods["ctrl"] {
		base = strings.ToLower(base)
	}
	aliases := map[string]string{"esc": "escape", "return": "enter", "pgup": "page-up", "pgdown": "page-down", "pgdn": "page-down"}
	if alias, ok := aliases[base]; ok {
		base = alias
	}
	if mods["ctrl"] && !mods["shift"] {
		alias := map[string]string{"i": "tab", "m": "enter", "[": "escape", "?": "backspace"}[base]
		if alias != "" {
			base = alias
			delete(mods, "ctrl")
		}
		if base == "space" || base == "`" {
			base = "@"
		}
	}
	if mods["shift"] && !mods["ctrl"] && utf8.RuneCountInString(base) == 1 {
		r, _ := utf8.DecodeRuneInString(base)
		if unicode.IsLetter(r) {
			base = strings.ToUpper(base)
			delete(mods, "shift")
		}
	}
	navigation := map[string]bool{"up": true, "down": true, "left": true, "right": true, "home": true, "end": true}
	valid := false
	switch {
	case mods["ctrl"] && mods["shift"]:
		valid = navigation[base]
	case mods["ctrl"]:
		valid = navigation[base] || base == "page-up" || base == "page-down" || (len(base) == 1 && strings.Contains("abcdefghijklmnopqrstuvwxyz@\\]^_", base))
	case mods["shift"]:
		valid = navigation[base] || base == "tab"
	default:
		valid = navigation[base] || map[string]bool{
			"enter": true, "backspace": true, "tab": true, "escape": true, "page-up": true, "page-down": true, "delete": true, "insert": true, "space": true,
			"f1": true, "f2": true, "f3": true, "f4": true, "f5": true, "f6": true, "f7": true, "f8": true, "f9": true, "f10": true,
			"f11": true, "f12": true, "f13": true, "f14": true, "f15": true, "f16": true, "f17": true, "f18": true, "f19": true, "f20": true,
		}[base]
		if utf8.RuneCountInString(base) == 1 {
			r, _ := utf8.DecodeRuneInString(base)
			valid = unicode.IsPrint(r)
		}
	}
	if !valid {
		return "", errors.New("unsupported key")
	}
	var prefix string
	for _, mod := range []string{"ctrl", "alt", "shift"} {
		if mods[mod] {
			prefix += mod + "+"
		}
	}
	return prefix + base, nil
}

func Printable(stroke string) bool {
	return stroke == "space" || (utf8.RuneCountInString(stroke) == 1 && !strings.Contains(stroke, "\n"))
}

type entry struct {
	action   string
	sequence Sequence
}
type Runtime struct {
	catalog  []Action
	bindings map[string][]entry
	document Document
	prefix   Sequence
	context  Context
	deadline time.Time
}

func New(catalog []Action) (*Runtime, error) {
	r := &Runtime{catalog: cloneCatalog(catalog)}
	if err := r.Apply(Document{Version: 1}); err != nil {
		return nil, err
	}
	return r, nil
}

func cloneSequences(sequences []Sequence) []Sequence {
	copy := make([]Sequence, len(sequences))
	for i, sequence := range sequences {
		copy[i] = append(Sequence(nil), sequence...)
	}
	return copy
}
func cloneCatalog(catalog []Action) []Action {
	copy := append([]Action(nil), catalog...)
	for i := range copy {
		copy[i].Defaults = cloneSequences(copy[i].Defaults)
	}
	return copy
}
func Clone(document Document) Document {
	copy := Document{Version: document.Version, Bindings: append([]Binding(nil), document.Bindings...)}
	for i := range copy.Bindings {
		copy.Bindings[i].Sequences = cloneSequences(copy.Bindings[i].Sequences)
	}
	return copy
}
func (r *Runtime) Catalog() []Action  { return cloneCatalog(r.catalog) }
func (r *Runtime) Document() Document { return Clone(r.document) }

// Validate checks the complete effective map, including inherited defaults.
func (r *Runtime) Validate(document Document) error { _, err := r.compile(document); return err }
func (r *Runtime) Apply(document Document) error {
	bindings, err := r.compile(document)
	if err != nil {
		return err
	}
	r.bindings = bindings
	r.document = Clone(document)
	r.Cancel()
	return nil
}
func bindingID(context, action string) string { return context + "/" + action }
func (r *Runtime) compile(document Document) (map[string][]entry, error) {
	if document.Version != 1 {
		return nil, errors.New("unsupported keymap version")
	}
	actions := make(map[string]Action, len(r.catalog))
	contexts := map[string]bool{}
	for _, action := range r.catalog {
		if _, err := diagnostics.NewID(action.ID); err != nil {
			return nil, errors.New("invalid action ID")
		}
		if _, err := diagnostics.NewID(action.Context); err != nil {
			return nil, errors.New("invalid context ID")
		}
		id := bindingID(action.Context, action.ID)
		if _, exists := actions[id]; exists || action.Label == "" {
			return nil, fmt.Errorf("invalid action declaration: %s", id)
		}
		if editing, exists := contexts[action.Context]; exists && editing != action.Editing {
			return nil, fmt.Errorf("inconsistent editing context: %s", action.Context)
		}
		contexts[action.Context] = action.Editing
		actions[id] = action
	}
	overrides := map[string][]Sequence{}
	for _, binding := range document.Bindings {
		id := bindingID(binding.Context, binding.Action)
		if _, ok := actions[id]; !ok {
			return nil, errors.New("unknown action or context")
		}
		if _, ok := overrides[id]; ok {
			return nil, fmt.Errorf("duplicate binding: %s", id)
		}
		overrides[id] = binding.Sequences
	}
	compiled := map[string][]entry{}
	for _, action := range r.catalog {
		sequences, ok := overrides[bindingID(action.Context, action.ID)]
		if !ok {
			sequences = action.Defaults
		}
		for _, sequence := range sequences {
			if len(sequence) == 0 {
				return nil, fmt.Errorf("empty sequence: %s/%s", action.Context, action.ID)
			}
			canonical := make(Sequence, len(sequence))
			for i, stroke := range sequence {
				normalized, err := Normalize(stroke)
				if err != nil || normalized == "ctrl+c" {
					return nil, fmt.Errorf("unsupported or reserved key: %s/%s", action.Context, action.ID)
				}
				canonical[i] = normalized
			}
			if action.Editing && Printable(canonical[0]) {
				return nil, fmt.Errorf("text-entry binding must start with a modified or non-text key: %s/%s", action.Context, action.ID)
			}
			for _, existing := range compiled[action.Context] {
				if prefixOf(existing.sequence, canonical) || prefixOf(canonical, existing.sequence) {
					return nil, fmt.Errorf("shortcut conflict in %s: %s and %s", action.Context, existing.action, action.ID)
				}
			}
			compiled[action.Context] = append(compiled[action.Context], entry{action.ID, canonical})
		}
	}
	return compiled, nil
}
func prefixOf(prefix, sequence Sequence) bool {
	if len(prefix) > len(sequence) {
		return false
	}
	for i := range prefix {
		if prefix[i] != sequence[i] {
			return false
		}
	}
	return true
}
func (r *Runtime) Effective(context, action string) []Sequence {
	var result []Sequence
	for _, entry := range r.bindings[context] {
		if entry.action == action {
			result = append(result, append(Sequence(nil), entry.sequence...))
		}
	}
	return result
}
func (r *Runtime) Cancel() { r.prefix = nil; r.deadline = time.Time{} }
func (r *Runtime) Sync(context Context, now time.Time) {
	if context != r.context || (!r.deadline.IsZero() && !now.Before(r.deadline)) {
		r.Cancel()
	}
	r.context = context
}
func (r *Runtime) Pending() Sequence   { return append(Sequence(nil), r.prefix...) }
func (r *Runtime) Deadline() time.Time { return r.deadline }
func (r *Runtime) Continuations() []string {
	var values []string
	if len(r.prefix) == 0 {
		return values
	}
	seen := map[string]bool{}
	for _, entry := range r.bindings[r.context.ID] {
		if prefixOf(r.prefix, entry.sequence) && len(entry.sequence) > len(r.prefix) {
			next := entry.sequence[len(r.prefix)]
			if !seen[next] {
				seen[next] = true
				values = append(values, next)
			}
		}
	}
	sort.Strings(values)
	return values
}

// Step consumes a normalized command stroke. On a mismatch it retries only
// the mismatching stroke, never the already consumed prefix.
func (r *Runtime) Step(context Context, stroke string, now time.Time) (action string, consumed bool) {
	r.Sync(context, now)
	for attempts := 0; attempts < 2; attempts++ {
		sequence := append(append(Sequence(nil), r.prefix...), stroke)
		for _, entry := range r.bindings[context.ID] {
			if !prefixOf(sequence, entry.sequence) {
				continue
			}
			if len(sequence) == len(entry.sequence) {
				r.Cancel()
				return entry.action, true
			}
			r.prefix = sequence
			r.deadline = now.Add(Timeout)
			return "", true
		}
		if len(r.prefix) == 0 {
			return "", !context.Editing || !Printable(stroke)
		}
		r.Cancel()
	}
	return "", true
}
