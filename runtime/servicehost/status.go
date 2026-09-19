package servicehost

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"unicode/utf8"
)

const MaxStatusDetailBytes = 1024

type HealthState uint8

const (
	HealthStarting HealthState = iota + 1
	HealthReady
	HealthDraining
	HealthDegraded
	HealthStopping
)

type Status struct {
	Epoch     string
	Revision  uint64
	State     HealthState
	Accepting bool
	Detail    string
}

type Cursor struct {
	Epoch    string
	Revision uint64
}

type StatusUpdate struct {
	Status Status
	Reset  bool
}

type statusHub struct {
	mu          sync.Mutex
	status      Status
	watchers    map[uint64]chan Status
	nextWatcher uint64
	maxWatchers int
	closed      bool
}

func newStatusHub(initial HealthState, accepting bool, detail string, maxWatchers int) (*statusHub, error) {
	if err := validateStatus(initial, detail); err != nil {
		return nil, err
	}
	epochBytes := make([]byte, 16)
	if _, err := rand.Read(epochBytes); err != nil {
		return nil, err
	}
	return &statusHub{
		status:      Status{Epoch: hex.EncodeToString(epochBytes), Revision: 1, State: initial, Accepting: accepting, Detail: detail},
		watchers:    make(map[uint64]chan Status),
		maxWatchers: maxWatchers,
	}, nil
}

func (h *statusHub) current() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

func (h *statusHub) update(state HealthState, accepting bool, detail string) (Status, error) {
	if err := validateStatus(state, detail); err != nil {
		return Status{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return Status{}, errors.New("servicehost: status hub is closed")
	}
	if h.status.State == state && h.status.Accepting == accepting && h.status.Detail == detail {
		return h.status, nil
	}
	h.status.Revision++
	h.status.State = state
	h.status.Accepting = accepting
	h.status.Detail = detail
	for id, watcher := range h.watchers {
		select {
		case watcher <- h.status:
		default:
			delete(h.watchers, id)
			close(watcher)
		}
	}
	return h.status, nil
}

func (h *statusHub) subscribe(cursor Cursor) (uint64, <-chan Status, *StatusUpdate, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, nil, nil, errors.New("servicehost: status hub is closed")
	}
	if len(h.watchers) >= h.maxWatchers {
		return 0, nil, nil, ErrBackpressure
	}
	h.nextWatcher++
	id := h.nextWatcher
	updates := make(chan Status, 1)
	h.watchers[id] = updates
	var initial *StatusUpdate
	if cursor.Epoch != h.status.Epoch || cursor.Revision != h.status.Revision {
		initial = &StatusUpdate{Status: h.status, Reset: true}
	}
	return id, updates, initial, nil
}

func (h *statusHub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if updates, ok := h.watchers[id]; ok {
		delete(h.watchers, id)
		close(updates)
	}
}

func (h *statusHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, watcher := range h.watchers {
		delete(h.watchers, id)
		close(watcher)
	}
}

func validateStatus(state HealthState, detail string) error {
	if state < HealthStarting || state > HealthStopping {
		return errors.New("servicehost: invalid health state")
	}
	if len(detail) > MaxStatusDetailBytes || !utf8.ValidString(detail) {
		return errors.New("servicehost: invalid status detail")
	}
	return nil
}
