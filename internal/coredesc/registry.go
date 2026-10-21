package coredesc

import (
	"fmt"
	"sort"

	"smartgate/internal/smartnode"
)

type Registry struct {
	descriptors map[smartnode.CoreID]Descriptor
	ports       map[int]smartnode.CoreID
}

func NewRegistry() *Registry {
	return &Registry{
		descriptors: map[smartnode.CoreID]Descriptor{},
		ports:       map[int]smartnode.CoreID{},
	}
}

func (registry *Registry) Register(descriptor Descriptor) error {
	if _, exists := registry.descriptors[descriptor.ID]; exists {
		return fmt.Errorf("core descriptor %q is already registered", descriptor.ID)
	}
	claimed := map[int]struct{}{}
	for _, port := range descriptorPorts(descriptor) {
		if _, exists := claimed[port]; exists {
			return fmt.Errorf("core descriptor %q reuses port %d", descriptor.ID, port)
		}
		if owner, exists := registry.ports[port]; exists {
			return fmt.Errorf("core descriptor %q port %d overlaps %q", descriptor.ID, port, owner)
		}
		claimed[port] = struct{}{}
	}
	for port := range claimed {
		registry.ports[port] = descriptor.ID
	}
	registry.descriptors[descriptor.ID] = descriptor
	return nil
}

func (registry *Registry) Lookup(id smartnode.CoreID) (Descriptor, bool) {
	descriptor, exists := registry.descriptors[id]
	return descriptor, exists
}

func (registry *Registry) List() []Descriptor {
	descriptors := make([]Descriptor, 0, len(registry.descriptors))
	for _, descriptor := range registry.descriptors {
		descriptors = append(descriptors, descriptor)
	}
	sort.Slice(descriptors, func(left, right int) bool {
		return descriptors[left].ID < descriptors[right].ID
	})
	return descriptors
}

func descriptorPorts(descriptor Descriptor) []int {
	return []int{descriptor.Control.Port, descriptor.Ports.TProxyBase, descriptor.Ports.DNSBase}
}
