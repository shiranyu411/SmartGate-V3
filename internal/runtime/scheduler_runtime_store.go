package runtime

import (
	"smartgate/internal/lockset"
	"smartgate/internal/state"
)

type schedulerRuntimeStore struct {
	store *state.Store
	locks *lockset.LockSet
}

func NewSchedulerRuntimeStore(store *state.Store, locks *lockset.LockSet) schedulerRuntimeStore {
	return schedulerRuntimeStore{store: store, locks: locks}
}

func (s schedulerRuntimeStore) LoadRuntime() (state.RuntimeState, error) {
	release, err := s.lock("scheduler-runtime-read")
	if err != nil {
		return state.RuntimeState{}, err
	}
	defer release()
	return s.store.LoadRuntime()
}

func (s schedulerRuntimeStore) SaveRuntime(runtimeState state.RuntimeState) error {
	release, err := s.lock("scheduler-runtime-write")
	if err != nil {
		return err
	}
	defer release()
	return s.store.SaveRuntime(runtimeState)
}

func (s schedulerRuntimeStore) MergeNodeHealth(health map[string]state.NodeHealth) error {
	release, err := s.lock("scheduler-node-health-merge")
	if err != nil {
		return err
	}
	defer release()
	return s.store.MergeNodeHealth(health)
}

func (s schedulerRuntimeStore) lock(name string) (func(), error) {
	if s.locks == nil {
		return func() {}, nil
	}
	session := s.locks.Session(name)
	releaseStore, err := session.Acquire(lockset.LevelStore)
	if err != nil {
		return nil, err
	}
	releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
	if err != nil {
		releaseStore()
		return nil, err
	}
	return func() {
		releaseRuntime()
		releaseStore()
	}, nil
}
