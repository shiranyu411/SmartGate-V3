package netgate

import (
	"context"
	"fmt"
	"os"
	"strings"

	"smartgate/internal/lockset"
)

type CleanupAction struct {
	Kind     string           `json:"kind"`
	ID       string           `json:"id"`
	OwnerTag string           `json:"owner_tag"`
	Comment  string           `json:"comment"`
	Commands []PlannedCommand `json:"commands"`
}

type CleanupPlan struct {
	OpID    string          `json:"op_id,omitempty"`
	Actions []CleanupAction `json:"actions"`
}

type CleanupResult struct {
	OpID    string   `json:"op_id,omitempty"`
	Logs    []string `json:"logs,omitempty"`
	Actions int      `json:"actions"`
	Steps   int      `json:"steps"`
}

type CleanupExecutor interface {
	Cleanup(ctx context.Context, session *lockset.Session) (CleanupPlan, error)
}

type CleanupPlanExecutor interface {
	ExecuteCleanup(ctx context.Context, session *lockset.Session, plan CleanupPlan) (CleanupResult, error)
}

type OwnerCleanup struct {
	Owners               *OwnerRegistry
	WorkerCleanupTargets []WorkerCleanupTarget
	Runner               Runner
	Execute              bool
}

func (c OwnerCleanup) Cleanup(ctx context.Context, session *lockset.Session) (CleanupPlan, error) {
	if err := ctx.Err(); err != nil {
		return CleanupPlan{}, err
	}
	releaseNet, err := session.Acquire(lockset.LevelNet)
	if err != nil {
		return CleanupPlan{}, err
	}
	defer releaseNet()

	dryRun, err := OpenWrtRescueCleanupDryRunPlan("rescue-cleanup", c.Owners, c.WorkerCleanupTargets)
	if err != nil {
		return CleanupPlan{}, err
	}
	return CleanupPlanFromDryRun(dryRun)
}

func (c OwnerCleanup) ExecuteCleanup(ctx context.Context, session *lockset.Session, plan CleanupPlan) (CleanupResult, error) {
	if !c.Execute {
		return CleanupResult{OpID: plan.OpID}, nil
	}
	if err := ctx.Err(); err != nil {
		return CleanupResult{}, err
	}
	releaseNet, err := session.Acquire(lockset.LevelNet)
	if err != nil {
		return CleanupResult{}, err
	}
	defer releaseNet()
	return ExecuteCleanupPlan(ctx, plan, c.Runner)
}

func ExecuteCleanupPlan(ctx context.Context, plan CleanupPlan, runner Runner) (CleanupResult, error) {
	if runner == nil {
		runner = ShellRunner{}
	}
	result := CleanupResult{OpID: plan.OpID}
	for _, action := range plan.Actions {
		if action.OwnerTag == "" || action.Comment == "" {
			return result, fmt.Errorf("cleanup action %s/%s missing owner metadata", action.Kind, action.ID)
		}
		if len(action.Commands) == 0 {
			return result, fmt.Errorf("cleanup action %s/%s has no commands", action.Kind, action.ID)
		}
		for _, command := range action.Commands {
			if !plannedCommandContains(command, action.Comment) {
				return result, fmt.Errorf("cleanup action %s/%s has command without owner match %q", action.Kind, action.ID, action.Comment)
			}
			log, err := runner.Run(ctx, command.Argv)
			result.Steps++
			if log != "" {
				result.Logs = append(result.Logs, log)
			}
			if err != nil {
				return result, err
			}
		}
		result.Actions++
	}
	return result, nil
}

func ShouldExecuteOpenWrtCleanup() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SMARTGATE_RESCUE_EXECUTE"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	body, err := os.ReadFile("/etc/openwrt_release")
	if err == nil && len(body) > 0 {
		return true
	}
	body, err = os.ReadFile("/etc/os-release")
	if err != nil {
		return false
	}
	contents := strings.ToLower(string(body))
	return strings.Contains(contents, "openwrt") || strings.Contains(contents, "immortalwrt")
}
