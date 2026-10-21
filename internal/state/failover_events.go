package state

import (
	"errors"
	"os"
	"time"
)

const MaxFailoverEvents = 200

type FailoverEvent struct {
	Time           time.Time `json:"time"`
	PoolID         string    `json:"pool_id"`
	Role           string    `json:"role"`
	From           string    `json:"from,omitempty"`
	To             string    `json:"to,omitempty"`
	GenerationFrom uint64    `json:"generation_from,omitempty"`
	GenerationTo   uint64    `json:"generation_to,omitempty"`
	Executor       string    `json:"executor,omitempty"`
	CutoverResult  string    `json:"cutover_result,omitempty"`
	Kind           string    `json:"kind"`
	Reason         string    `json:"reason"`
	Source         string    `json:"source,omitempty"`
	Status         string    `json:"status"`
	Error          string    `json:"error,omitempty"`
}

type FailoverEventStore struct {
	layout Layout
}

func NewFailoverEventStore(layout Layout) FailoverEventStore {
	return FailoverEventStore{layout: layout}
}

func (s FailoverEventStore) List() ([]FailoverEvent, error) {
	var events []FailoverEvent
	err := loadJSON(s.layout.FailoverEventsPath(), &events)
	if errors.Is(err, os.ErrNotExist) {
		return []FailoverEvent{}, nil
	}
	return events, err
}

func (s FailoverEventStore) Append(events []FailoverEvent) ([]FailoverEvent, error) {
	if len(events) == 0 {
		return s.List()
	}
	existing, err := s.List()
	if err != nil {
		return nil, err
	}
	existing = append(existing, events...)
	if len(existing) > MaxFailoverEvents {
		existing = existing[len(existing)-MaxFailoverEvents:]
	}
	_, err = saveJSONAtomicIfChanged(s.layout.FailoverEventsPath(), existing, 0o600)
	return existing, err
}
