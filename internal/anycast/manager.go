package anycast

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
)

type LinkState struct {
	Managed   bool
	Addresses []netip.Addr
}

type Kernel interface {
	EnsureLink(name string) (LinkState, error)
	AddAddress(name string, address netip.Addr) error
	DeleteAddress(name string, address netip.Addr) error
	DeleteLink(name string) error
}

type Manager struct {
	kernel  Kernel
	name    string
	managed bool
	owned   map[netip.Addr]struct{}
}

func NewManager(kernel Kernel, name string) *Manager {
	return &Manager{kernel: kernel, name: name, owned: make(map[netip.Addr]struct{})}
}

func (m *Manager) Name() string {
	return m.name
}

func (m *Manager) Add(wanted []netip.Addr) ([]netip.Addr, error) {
	state, err := m.kernel.EnsureLink(m.name)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %w", m.name, err)
	}

	m.managed = state.Managed
	if m.managed {
		for _, address := range state.Addresses {
			m.owned[address] = struct{}{}
		}
	}

	var added []netip.Addr

	for _, address := range wanted {
		if slices.Contains(state.Addresses, address) {
			continue
		}

		if err := m.kernel.AddAddress(m.name, address); err != nil {
			return added, fmt.Errorf("add %s to %s: %w", address, m.name, err)
		}

		m.owned[address] = struct{}{}
		added = append(added, address)
	}

	return added, nil
}

func (m *Manager) Prune(wanted []netip.Addr) ([]netip.Addr, error) {
	var removed []netip.Addr

	var failures []error

	for _, address := range m.Owned() {
		if slices.Contains(wanted, address) {
			continue
		}

		if err := m.kernel.DeleteAddress(m.name, address); err != nil {
			failures = append(failures, fmt.Errorf("remove %s from %s: %w", address, m.name, err))
			continue
		}

		delete(m.owned, address)
		removed = append(removed, address)
	}

	return removed, errors.Join(failures...)
}

func (m *Manager) Close() ([]netip.Addr, error) {
	removed, err := m.Prune(nil)
	if err != nil || !m.managed {
		return removed, err
	}

	if err := m.kernel.DeleteLink(m.name); err != nil {
		return removed, fmt.Errorf("remove interface %s: %w", m.name, err)
	}

	m.managed = false
	return removed, nil
}

func (m *Manager) Owned() []netip.Addr {
	addresses := make([]netip.Addr, 0, len(m.owned))
	for address := range m.owned {
		addresses = append(addresses, address)
	}

	slices.SortFunc(addresses, netip.Addr.Compare)
	return addresses
}
