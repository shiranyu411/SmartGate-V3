package failover

import (
	"time"
)

type Kind string

const (
	KindSwitch     Kind = "switch"
	KindFailClosed Kind = "fail_closed"
	KindRecovery   Kind = "recovery"
	KindNoop       Kind = "noop"
)

type PoolHealth struct {
	PoolID      string   `json:"pool_id"`
	Role        string   `json:"role"`
	NoCandidate string   `json:"no_candidate"`
	Current     string   `json:"current,omitempty"`
	Candidates  []string `json:"candidates"`
	Healthy     []string `json:"healthy"`
}

type Decision struct {
	PoolID string    `json:"pool_id"`
	Role   string    `json:"role"`
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
	Kind   Kind      `json:"kind"`
	Reason string    `json:"reason"`
	Time   time.Time `json:"time"`
}

func Decide(pool PoolHealth, now time.Time) Decision {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	healthy := firstHealthyCandidate(pool.Candidates, pool.Healthy)
	if healthy == "" {
		switch pool.NoCandidate {
		case "block":
			return Decision{PoolID: pool.PoolID, Role: pool.Role, From: pool.Current, To: "Block", Kind: KindFailClosed, Reason: "no_healthy_candidate_block", Time: now}
		case "hold":
			return Decision{PoolID: pool.PoolID, Role: pool.Role, From: pool.Current, To: pool.Current, Kind: KindNoop, Reason: "no_healthy_candidate_hold", Time: now}
		default:
			return Decision{PoolID: pool.PoolID, Role: pool.Role, From: pool.Current, To: "Direct", Kind: KindSwitch, Reason: "no_healthy_candidate_direct", Time: now}
		}
	}
	if pool.Current == "" {
		return Decision{PoolID: pool.PoolID, Role: pool.Role, To: healthy, Kind: KindSwitch, Reason: "select_first_healthy", Time: now}
	}
	if pool.Current == "Block" || pool.Current == "Direct" {
		return Decision{PoolID: pool.PoolID, Role: pool.Role, From: pool.Current, To: healthy, Kind: KindRecovery, Reason: "healthy_candidate_recovered", Time: now}
	}
	if pool.Current == healthy {
		return Decision{PoolID: pool.PoolID, Role: pool.Role, From: pool.Current, To: pool.Current, Kind: KindNoop, Reason: "current_is_healthy", Time: now}
	}
	if contains(pool.Healthy, pool.Current) {
		return Decision{PoolID: pool.PoolID, Role: pool.Role, From: pool.Current, To: pool.Current, Kind: KindNoop, Reason: "current_is_healthy", Time: now}
	}
	return Decision{PoolID: pool.PoolID, Role: pool.Role, From: pool.Current, To: healthy, Kind: KindSwitch, Reason: "current_unhealthy_switch_next", Time: now}
}

func firstHealthyCandidate(candidates, healthy []string) string {
	for _, candidate := range candidates {
		if contains(healthy, candidate) {
			return candidate
		}
	}
	return ""
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
