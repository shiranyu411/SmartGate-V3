package lockset

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelRescueGate Level = iota
	LevelCommitLane
	LevelStore
	LevelSnapshot
	LevelRuntime
	LevelNet
)

var levelNames = map[Level]string{
	LevelRescueGate: "L0 RescueGate",
	LevelCommitLane: "L1 CommitLane",
	LevelStore:      "L2 StoreLock",
	LevelSnapshot:   "L3 SnapshotLock",
	LevelRuntime:    "L4 RuntimeLock",
	LevelNet:        "L5 NetLock",
}

type LockSet struct {
	rescue   sync.Mutex
	commit   sync.Mutex
	store    sync.Mutex
	snapshot sync.Mutex
	runtime  sync.Mutex
	net      sync.Mutex
}

func New() *LockSet {
	return &LockSet{}
}

type Session struct {
	locks *LockSet
	opID  string
	held  []Level
}

func (ls *LockSet) Session(opID string) *Session {
	return &Session{locks: ls, opID: opID}
}

func (s *Session) Acquire(level Level) (func(), error) {
	return s.AcquireContext(context.Background(), level)
}

func (s *Session) AcquireContext(ctx context.Context, level Level) (func(), error) {
	if len(s.held) > 0 {
		last := s.held[len(s.held)-1]
		if level < last {
			return nil, fmt.Errorf("lock order violation: %s cannot acquire %s after %s", s.opID, levelNames[level], levelNames[last])
		}
	}
	switch level {
	case LevelRescueGate:
		if !s.locks.rescue.TryLock() {
			return s.waitForLock(ctx, level)
		}
	case LevelCommitLane:
		if !s.locks.commit.TryLock() {
			return s.waitForLock(ctx, level)
		}
	case LevelStore:
		if !s.locks.store.TryLock() {
			return s.waitForLock(ctx, level)
		}
	case LevelSnapshot:
		if !s.locks.snapshot.TryLock() {
			return s.waitForLock(ctx, level)
		}
	case LevelRuntime:
		if !s.locks.runtime.TryLock() {
			return s.waitForLock(ctx, level)
		}
	case LevelNet:
		if !s.locks.net.TryLock() {
			return s.waitForLock(ctx, level)
		}
	default:
		return nil, errors.New("unknown lock level")
	}
	s.held = append(s.held, level)
	return s.release(level), nil
}

func (s *Session) waitForLock(ctx context.Context, level Level) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if s.tryLock(level) {
				s.held = append(s.held, level)
				return s.release(level), nil
			}
		}
	}
}

func (s *Session) tryLock(level Level) bool {
	switch level {
	case LevelRescueGate:
		return s.locks.rescue.TryLock()
	case LevelCommitLane:
		return s.locks.commit.TryLock()
	case LevelStore:
		return s.locks.store.TryLock()
	case LevelSnapshot:
		return s.locks.snapshot.TryLock()
	case LevelRuntime:
		return s.locks.runtime.TryLock()
	case LevelNet:
		return s.locks.net.TryLock()
	default:
		return false
	}
}

func (s *Session) release(level Level) func() {
	return func() {
		if len(s.held) == 0 {
			return
		}
		last := s.held[len(s.held)-1]
		if last != level {
			panic(fmt.Sprintf("unlock order violation: expected %s, got %s", levelNames[last], levelNames[level]))
		}
		s.held = s.held[:len(s.held)-1]
		switch level {
		case LevelRescueGate:
			s.locks.rescue.Unlock()
		case LevelCommitLane:
			s.locks.commit.Unlock()
		case LevelStore:
			s.locks.store.Unlock()
		case LevelSnapshot:
			s.locks.snapshot.Unlock()
		case LevelRuntime:
			s.locks.runtime.Unlock()
		case LevelNet:
			s.locks.net.Unlock()
		}
	}
}

func DescribeHierarchy() string {
	ordered := []Level{
		LevelRescueGate,
		LevelCommitLane,
		LevelStore,
		LevelSnapshot,
		LevelRuntime,
		LevelNet,
	}
	lines := make([]string, 0, len(ordered)+2)
	lines = append(lines, "SmartGate lock hierarchy:")
	for _, level := range ordered {
		lines = append(lines, " - "+levelNames[level])
	}
	lines = append(lines, "Hard rule: acquire only in ascending order; leaf operations must not escalate into CommitLane.")
	return strings.Join(lines, "\n") + "\n"
}
