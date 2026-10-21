package commitlane

import (
	"sort"
	"sync"
	"time"

	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type CutoverProgress struct {
	Pool           string `json:"pool"`
	Stage          string `json:"stage"`
	GenerationFrom uint64 `json:"generation_from"`
	GenerationTo   uint64 `json:"generation_to"`
}

type DrainProgress struct {
	Pool          string                  `json:"pool"`
	OldGeneration uint64                  `json:"old_generation"`
	Remaining     int                     `json:"remaining"`
	Deadline      time.Time               `json:"deadline"`
	Status        string                  `json:"status"`
	LastError     string                  `json:"last_error,omitempty"`
	RetryCount    int                     `json:"retry_count"`
	Binding       state.PoolBinding       `json:"-"`
	Dispatch      state.PoolDispatchGraph `json:"-"`
	Path          *worker.BindingPath     `json:"-"`
}

type CutoverTracker struct {
	mu       sync.RWMutex
	cutovers map[string]CutoverProgress
	drains   map[string]DrainProgress
}

func NewCutoverTracker() *CutoverTracker {
	return &CutoverTracker{cutovers: map[string]CutoverProgress{}, drains: map[string]DrainProgress{}}
}

func (tracker *CutoverTracker) Start(pool string, generationFrom, generationTo uint64) error {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if _, exists := tracker.cutovers[pool]; exists {
		return CutoverConflictError{Pool: pool, Reason: "cutover already in progress"}
	}
	if drain, exists := tracker.drains[pool]; exists {
		return CutoverConflictError{Pool: pool, Reason: "generation drain is still active", OldGeneration: drain.OldGeneration}
	}
	tracker.cutovers[pool] = CutoverProgress{Pool: pool, Stage: "preparing", GenerationFrom: generationFrom, GenerationTo: generationTo}
	return nil
}

func (tracker *CutoverTracker) Stage(pool, stage string) {
	tracker.mu.Lock()
	progress, ok := tracker.cutovers[pool]
	if ok {
		progress.Stage = stage
		tracker.cutovers[pool] = progress
	}
	tracker.mu.Unlock()
}

func (tracker *CutoverTracker) Finish(pool string) {
	tracker.mu.Lock()
	delete(tracker.cutovers, pool)
	tracker.mu.Unlock()
}

func (tracker *CutoverTracker) BeginDrain(progress DrainProgress) {
	if progress.Status == "" {
		progress.Status = state.CutoverDrainStatusDraining
	}
	tracker.mu.Lock()
	delete(tracker.cutovers, progress.Pool)
	tracker.drains[progress.Pool] = progress
	tracker.mu.Unlock()
}

func (tracker *CutoverTracker) UpdateDrain(pool string, remaining int, status, lastError string, retryCount int) {
	tracker.mu.Lock()
	progress, ok := tracker.drains[pool]
	if ok {
		progress.Remaining = remaining
		progress.Status = status
		progress.LastError = lastError
		progress.RetryCount = retryCount
		tracker.drains[pool] = progress
	}
	tracker.mu.Unlock()
}

func (tracker *CutoverTracker) FinishDrain(pool string) {
	tracker.mu.Lock()
	delete(tracker.drains, pool)
	tracker.mu.Unlock()
}

func (tracker *CutoverTracker) Snapshot() ([]CutoverProgress, []DrainProgress) {
	tracker.mu.RLock()
	cutovers := make([]CutoverProgress, 0, len(tracker.cutovers))
	for _, progress := range tracker.cutovers {
		cutovers = append(cutovers, progress)
	}
	drains := make([]DrainProgress, 0, len(tracker.drains))
	for _, progress := range tracker.drains {
		drains = append(drains, progress)
	}
	tracker.mu.RUnlock()
	sort.Slice(cutovers, func(i, j int) bool { return cutovers[i].Pool < cutovers[j].Pool })
	sort.Slice(drains, func(i, j int) bool { return drains[i].Pool < drains[j].Pool })
	return cutovers, drains
}

func (tracker *CutoverTracker) Clear() {
	tracker.mu.Lock()
	tracker.cutovers = map[string]CutoverProgress{}
	tracker.drains = map[string]DrainProgress{}
	tracker.mu.Unlock()
}
