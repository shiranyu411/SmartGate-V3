package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"smartgate/internal/smartnode"
)

const MaxControlStateJSONBytes = 8 << 20

type JSONResourceLimitError struct {
	Path  string
	Limit int64
}

func (e *JSONResourceLimitError) Error() string {
	return fmt.Sprintf("state json %s exceeds %d bytes", filepath.Base(e.Path), e.Limit)
}

type Store struct {
	layout Layout
}

func NewStore(layout Layout) *Store {
	return &Store{layout: layout}
}

func (s *Store) LoadSpec() (SpecState, error) {
	var spec SpecState
	err := loadJSON(s.layout.SpecPath("spec"), &spec)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultSpecState(), nil
	}
	if err != nil {
		return SpecState{}, err
	}
	return NormalizeSpecState(spec), nil
}

func (s *Store) SaveSpec(spec SpecState) (bool, error) {
	return saveJSONAtomicIfChanged(s.layout.SpecPath("spec"), NormalizeSpecState(spec), 0o600)
}

func NormalizeSpecState(spec SpecState) SpecState {
	spec.SmartNodes, _ = smartnode.WithDefaultCoreChoices(spec.SmartNodes)
	legacySchema := spec.SchemaVersion < 3
	if spec.BusinessPools == nil {
		spec.BusinessPools = DefaultBusinessPools()
		if legacySchema {
			for i := range spec.BusinessPools {
				spec.BusinessPools[i].Policy.Cutover = CutoverStrict
			}
		}
	}
	for index := range spec.BusinessPools {
		pool := &spec.BusinessPools[index]
		if pool.BuiltinOrigin == "" {
			pool.BuiltinOrigin = pool.Role
		}
		if pool.Policy == (PoolPolicy{}) {
			pool.Policy = TemplatePoolPolicy(pool.BuiltinOrigin)
			if legacySchema && pool.Policy.Cutover != "" {
				pool.Policy.Cutover = CutoverStrict
			}
		}
	}
	if spec.Fabric == (FabricSpec{}) {
		spec.Fabric = DefaultFabricSpec()
	}
	if legacySchema && spec.Rules == nil {
		spec.Rules = DefaultRules()
	}
	spec.Rules = NormalizeRuleCatalogForPools(spec.Rules, spec.BusinessPools)
	if spec.SchemaVersion < 3 {
		spec.SchemaVersion = 3
	}
	for index := range spec.Scheduler {
		spec.Scheduler[index] = NormalizeSchedulerTaskSpec(spec.Scheduler[index])
	}
	return spec
}

// PersistDefaultCoreChoices upgrades legacy saved nodes at startup, before
// background tasks run. It does not apply desired configuration to live routes.
func (s *Store) PersistDefaultCoreChoices() error {
	var spec SpecState
	if err := loadJSON(s.layout.SpecPath("spec"), &spec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	nodes, changed := smartnode.WithDefaultCoreChoices(spec.SmartNodes)
	if !changed {
		return nil
	}
	spec.SmartNodes = nodes
	_, err := s.SaveSpec(spec)
	return err
}

func (s *Store) LoadRuntime() (RuntimeState, error) {
	var runtime RuntimeState
	err := loadJSON(s.layout.RuntimePath("runtime"), &runtime)
	if errors.Is(err, os.ErrNotExist) {
		return RuntimeState{}, nil
	}
	return runtime, err
}

func (s *Store) SaveRuntime(runtime RuntimeState) error {
	_, err := saveJSONAtomicIfChanged(s.layout.RuntimePath("runtime"), runtime, 0o600)
	return err
}

func (s *Store) MergeNodeHealth(health map[string]NodeHealth) error {
	if len(health) == 0 {
		return nil
	}
	runtimeState, err := s.LoadRuntime()
	if err != nil {
		return err
	}
	if runtimeState.NodeHealth == nil {
		runtimeState.NodeHealth = map[string]NodeHealth{}
	}
	for nodeID, item := range health {
		if item.WatchdogStatus == "" {
			previous := runtimeState.NodeHealth[nodeID]
			item.WatchdogStatus = previous.WatchdogStatus
			item.WatchdogUpdatedAt = previous.WatchdogUpdatedAt
			item.WatchdogContext = previous.WatchdogContext
			item.ConsecutiveFailures = previous.ConsecutiveFailures
		}
		runtimeState.NodeHealth[nodeID] = item
	}
	return s.SaveRuntime(runtimeState)
}

func (s *Store) AppendProviderLogs(logs []ProviderRefreshLog, max int) error {
	if len(logs) == 0 {
		return nil
	}
	runtimeState, err := s.LoadRuntime()
	if err != nil {
		return err
	}
	runtimeState.ProviderLogs = append(runtimeState.ProviderLogs, logs...)
	if max > 0 && len(runtimeState.ProviderLogs) > max {
		runtimeState.ProviderLogs = runtimeState.ProviderLogs[len(runtimeState.ProviderLogs)-max:]
	}
	return s.SaveRuntime(runtimeState)
}

func loadJSON(path string, v any) error {
	return loadJSONBounded(path, v, MaxControlStateJSONBytes)
}

func loadJSONBounded(path string, v any, maxBytes int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > maxBytes {
		return &JSONResourceLimitError{Path: path, Limit: maxBytes}
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	b, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if int64(len(b)) > maxBytes {
		return &JSONResourceLimitError{Path: path, Limit: maxBytes}
	}
	return json.Unmarshal(b, v)
}

func saveJSONAtomicIfChanged(path string, v any, perm os.FileMode) (bool, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return false, err
	}
	if len(b) > MaxControlStateJSONBytes {
		return false, &JSONResourceLimitError{Path: path, Limit: MaxControlStateJSONBytes}
	}
	if old, err := readFileBounded(path, MaxControlStateJSONBytes); err == nil && bytes.Equal(old, b) {
		_, err := chmodFileIfNeeded(path, perm)
		return false, err
	}
	return true, saveJSONDurable(path, v, perm)
}

func chmodFileIfNeeded(path string, perm os.FileMode) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	const controlledMode = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if info.Mode()&controlledMode == perm&controlledMode {
		return false, nil
	}
	return true, os.Chmod(path, perm)
}

func readFileBounded(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, &JSONResourceLimitError{Path: path, Limit: maxBytes}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > maxBytes {
		return nil, &JSONResourceLimitError{Path: path, Limit: maxBytes}
	}
	return body, nil
}
