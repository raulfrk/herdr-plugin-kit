// Package shell provides the single Bubble Tea lifecycle used by kit surfaces.
package shell

import (
	"context"
	"errors"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
)

type Event interface{ shellEvent() }

type KeyCode string

const (
	KeyEnter     KeyCode = "enter"
	KeyBackspace KeyCode = "backspace"
	KeyTab       KeyCode = "tab"
	KeyEscape    KeyCode = "escape"
	KeyUp        KeyCode = "up"
	KeyDown      KeyCode = "down"
	KeyLeft      KeyCode = "left"
	KeyRight     KeyCode = "right"
	KeyHome      KeyCode = "home"
	KeyEnd       KeyCode = "end"
	KeyPageUp    KeyCode = "page-up"
	KeyPageDown  KeyCode = "page-down"
	KeyDelete    KeyCode = "delete"
	KeySpace     KeyCode = "space"
	KeyCtrlC     KeyCode = "ctrl-c"
)

type KeyEvent struct {
	Code KeyCode
	Alt  bool
}

func (KeyEvent) shellEvent() {}

type TextEvent struct {
	Text  string
	Paste bool
	Alt   bool
}

func (TextEvent) shellEvent() {}

type FocusEvent struct{ Focused bool }

func (FocusEvent) shellEvent() {}

type ResizeEvent struct {
	Layout     responsive.Layout
	Generation uint64
}

func (ResizeEvent) shellEvent() {}

type TimerEvent struct {
	Code       EventCode
	Generation uint64
}

func (TimerEvent) shellEvent() {}

type ResultEvent struct {
	Key         RequestKey
	Generation  uint64
	Correlation diagnostics.ID
	Result      WorkResult
}

func (ResultEvent) shellEvent() {}

type RequestKey struct{ id diagnostics.ID }
type EventCode struct{ id diagnostics.ID }

func NewRequestKey(value string) (RequestKey, error) {
	id, err := diagnostics.NewID(value)
	if err != nil {
		return RequestKey{}, err
	}
	return RequestKey{id: id}, nil
}

func NewEventCode(value string) (EventCode, error) {
	id, err := diagnostics.NewID(value)
	if err != nil {
		return EventCode{}, err
	}
	return EventCode{id: id}, nil
}

func (key RequestKey) String() string { return key.id.String() }
func (code EventCode) String() string { return code.id.String() }

type WorkResult struct {
	Value any
	Code  diagnostics.OutcomeCode
	Err   error
}

type Work func(context.Context) WorkResult

type effectKind uint8

const (
	_ effectKind = iota
	effectWork
	effectTimer
	effectQuit
)

type Effect struct {
	kind                effectKind
	requestKey          RequestKey
	eventCode           EventCode
	generation          uint64
	correlation         diagnostics.ID
	previousCorrelation diagnostics.ID
	delay               time.Duration
	work                Work
}

func (effect Effect) RequestKey() RequestKey { return effect.requestKey }
func (effect Effect) EventCode() EventCode   { return effect.eventCode }

func Quit() Effect { return Effect{kind: effectQuit} }

type EventContext struct {
	start func(RequestKey, Work) (Effect, error)
	after func(time.Duration, EventCode) (Effect, error)
}

func (context EventContext) Start(key RequestKey, work Work) (Effect, error) {
	if context.start == nil {
		return Effect{}, errors.New("event context cannot start work")
	}
	return context.start(key, work)
}

func (context EventContext) After(delay time.Duration, code EventCode) (Effect, error) {
	if context.after == nil {
		return Effect{}, errors.New("event context cannot schedule timer")
	}
	return context.after(delay, code)
}
