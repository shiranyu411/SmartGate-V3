package netgate

import "fmt"

const (
	KindTunInterface   = "tun"
	KindDNSHijack      = "dns-hijack"
	KindDoTBlock       = "dot-block"
	KindDNSMasq        = "dnsmasq"
	KindFirewallHook   = "firewall-hook"
	KindFabricTable    = "fabric/mark-table"
	KindFabricPoolSet  = "fabric/pool-set"
	KindFabricGeoSet   = "fabric/geo-set"
	KindFabricDispatch = "fabric/dispatch"
	KindFabricRoute    = "fabric/pool-route"
	KindWorkerConfig   = "worker-config"
	KindWorkerService  = "worker-service"
)

type OwnedResource struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	OwnerTag string `json:"owner_tag"`
	Comment  string `json:"comment"`
}

func (r OwnedResource) IsOwnerTagged() bool {
	return r.OwnerTag != "" && r.Comment == "owner="+r.OwnerTag
}

type Plan struct {
	OpID      string          `json:"op_id"`
	Resources []OwnedResource `json:"resources"`
}

type WorkerCleanupTarget struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func NewPlan(opID string) Plan {
	return Plan{OpID: opID}
}

func (p *Plan) Add(owners *OwnerRegistry, kind, id string) {
	p.Resources = append(p.Resources, OwnedResource{
		Kind:     kind,
		ID:       id,
		OwnerTag: owners.Tag(kind, id),
		Comment:  owners.Comment(kind, id),
	})
}

func (p Plan) RequireOwned(kind, id string) (OwnedResource, error) {
	for _, resource := range p.Resources {
		if resource.Kind == kind && resource.ID == id {
			if !resource.IsOwnerTagged() {
				return OwnedResource{}, fmt.Errorf("resource %s/%s is missing owner metadata", kind, id)
			}
			return resource, nil
		}
	}
	return OwnedResource{}, fmt.Errorf("resource %s/%s is not in plan", kind, id)
}

func RuntimePlan(opID string, owners *OwnerRegistry) Plan {
	plan := NewPlan(opID)
	plan.Add(owners, KindTunInterface, "tun-sb")
	plan.Add(owners, KindDNSHijack, "lan-ipv4-53")
	plan.Add(owners, KindDoTBlock, "lan-ipv4-853")
	plan.Add(owners, KindDNSMasq, "wan-cpe-upstream")
	plan.Add(owners, KindFirewallHook, "fw4-reconcile")
	return plan
}

func RescuePlan(opID string, owners *OwnerRegistry, workers []WorkerCleanupTarget) Plan {
	plan := RuntimePlan(opID, owners)
	plan.Add(owners, KindFirewallHook, "fabric-reconcile")
	plan.Add(owners, KindDNSMasq, "smartgate-upstream")
	plan.Add(owners, KindFabricTable, "smartgate")
	plan.Add(owners, KindFabricRoute, "protocol-186")
	for _, worker := range workers {
		plan.Add(owners, KindWorkerService, worker.ID)
	}
	return plan
}
