package worker

import (
	"context"
	"errors"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StateDraining = "draining"
	StateFailed   = "failed"
)

type CompileRequest struct {
	Worker     state.WorkerTarget
	Executors  []state.NodeExecutor
	Bindings   []state.PoolBinding
	Identities map[string]smartnode.IdentitySource
	Generation time.Time
}

type Candidate struct {
	OpID         string           `json:"op_id"`
	WorkerID     string           `json:"worker_id"`
	Core         smartnode.CoreID `json:"core"`
	Path         string           `json:"path"`
	Size         int              `json:"size"`
	ConfigSHA256 string           `json:"config_sha256"`
	CheckLog     string           `json:"check_log,omitempty"`
	CheckOK      bool             `json:"check_ok"`
	CreatedAt    time.Time        `json:"created_at"`
	ReadyPorts   []string         `json:"ready_ports,omitempty"`
}

type Backup struct {
	WorkerID   string           `json:"worker_id"`
	Core       smartnode.CoreID `json:"core"`
	Path       string           `json:"path,omitempty"`
	Existed    bool             `json:"existed"`
	WasActive  bool             `json:"was_active"`
	ReadyPorts []string         `json:"ready_ports,omitempty"`
}

type DelayResult struct {
	DelayMS int
	URL     string
}

type Connection struct {
	ID       string
	Metadata map[string]any
	Upload   float64
	Download float64
	Chains   []string
	Start    string
}

type Activation struct {
	WorkerID   string `json:"worker_id"`
	ConfigPath string `json:"config_path,omitempty"`
	Restarted  bool   `json:"restarted"`
	Log        string `json:"log,omitempty"`
}

type Event struct {
	At         time.Time `json:"at"`
	Worker     string    `json:"worker"`
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	Message    string    `json:"message,omitempty"`
	Diagnostic string    `json:"diagnostic,omitempty"`
}

type Stats struct {
	RSSKB       int `json:"rss_kb,omitempty"`
	FD          int `json:"fd,omitempty"`
	Connections int `json:"connections,omitempty"`
}

type Status struct {
	ID        string               `json:"id"`
	Core      smartnode.CoreID     `json:"core"`
	Version   string               `json:"version,omitempty"`
	State     string               `json:"state"`
	Executors []state.NodeExecutor `json:"executors"`
	Pools     []string             `json:"pools"`
	Stats     Stats                `json:"stats,omitempty"`
	LastEvent *Event               `json:"last_event,omitempty"`
}

type Prepared struct {
	OpID          string               `json:"op_id"`
	Candidates    map[string]Candidate `json:"candidates"`
	Targets       []state.WorkerTarget `json:"targets"`
	ActiveTargets []string             `json:"active_targets"`
	Executors     []state.NodeExecutor `json:"executors"`
	Bindings      []state.PoolBinding  `json:"bindings"`
}

type Checkpoint struct {
	Backups  map[string]Backup             `json:"backups"`
	Targets  map[string]state.WorkerTarget `json:"targets"`
	Statuses map[string]Status             `json:"statuses"`
	Prepared Prepared                      `json:"prepared"`
}

type ApplyResult struct {
	Workers []Activation `json:"workers"`
}

type Adapter interface {
	Core() smartnode.CoreID
	Descriptor() Descriptor
	Prepare(context.Context, string, CompileRequest) (Candidate, error)
	Verify(Candidate) error
	Install(context.Context, Candidate) (Activation, error)
	Activate(context.Context, Candidate) (Activation, error)
	Start(context.Context, state.WorkerTarget) (Activation, error)
	Backup(string, string) (Backup, error)
	Restore(context.Context, Backup) (Activation, error)
	Stop(context.Context, string) error
	Probe(context.Context, string) error
	RemoveCandidate(Candidate) error
}

type Failure struct {
	WorkerID string
	Core     smartnode.CoreID
	Pools    []string
	Err      error
}

type FailureHandler func(context.Context, Failure)

var ErrConnectionCountUnsupported = errors.New("worker connection count is unsupported")
var ErrConnectionObservationUnsupported = errors.New("worker connection observation is unsupported")
var ErrRouteDelayUnsupported = errors.New("worker route delay is unsupported")

type BindingPath struct {
	Worker   state.WorkerTarget  `json:"worker"`
	Route    state.WorkerRoute   `json:"route"`
	TProxy   state.WorkerInbound `json:"tproxy"`
	DNS      state.WorkerInbound `json:"dns"`
	Executor state.NodeExecutor  `json:"executor"`
}

type RouteAdapter interface {
	SelectRoute(context.Context, BindingPath) error
	VerifyRoute(context.Context, BindingPath) error
	DelayRoute(context.Context, BindingPath, string, time.Duration) (DelayResult, error)
	ListRouteConnections(context.Context, BindingPath) ([]Connection, error)
	CloseRouteConnection(context.Context, BindingPath, string) error
	CountRouteConnections(context.Context, BindingPath) (int, error)
	CloseRouteConnections(context.Context, BindingPath) (int, error)
}

// RouteSelectionVerifier checks that the Worker's live selector points at the
// executor a BindingPath names without sending traffic through the upstream
// node. VerifyRoute additionally probes the upstream; callers that hold the
// control-plane locks (the data-plane supervisor's post-restart reconcile)
// must use this cheaper check so an unreachable node cannot stall the
// management API. End-to-end node reachability belongs to the failover
// watchdog.
type RouteSelectionVerifier interface {
	VerifyRouteSelection(context.Context, BindingPath) error
}
