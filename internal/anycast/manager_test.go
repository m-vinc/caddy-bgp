package anycast

import (
	"errors"
	"net/netip"
	"slices"
	"testing"
)

var (
	first  = netip.MustParseAddr("198.51.100.1")
	second = netip.MustParseAddr("2001:db8::1")
	other  = netip.MustParseAddr("198.51.100.99")
)

type fakeLink struct {
	managed   bool
	addresses []netip.Addr
}

type fakeKernel struct {
	links  map[string]*fakeLink
	refuse netip.Addr
}

func newFakeKernel() *fakeKernel {
	return &fakeKernel{links: make(map[string]*fakeLink)}
}

func (k *fakeKernel) EnsureLink(name string) (LinkState, error) {
	link, exists := k.links[name]
	if !exists {
		link = &fakeLink{managed: true}
		k.links[name] = link
	}

	return LinkState{Managed: link.managed, Addresses: slices.Clone(link.addresses)}, nil
}

func (k *fakeKernel) AddAddress(name string, address netip.Addr) error {
	if address == k.refuse {
		return errors.New("refused")
	}

	link := k.links[name]
	if slices.Contains(link.addresses, address) {
		return errors.New("exists")
	}

	link.addresses = append(link.addresses, address)
	return nil
}

func (k *fakeKernel) DeleteAddress(name string, address netip.Addr) error {
	link := k.links[name]
	index := slices.Index(link.addresses, address)
	if index < 0 {
		return errors.New("missing")
	}

	link.addresses = slices.Delete(link.addresses, index, index+1)
	return nil
}

func (k *fakeKernel) DeleteLink(name string) error {
	delete(k.links, name)
	return nil
}

func TestManagerCreatesAndRemovesItsLink(t *testing.T) {
	kernel := newFakeKernel()
	manager := NewManager(kernel, "cbgp0")
	added, err := manager.Add([]netip.Addr{first, second})
	if err != nil || !slices.Equal(added, []netip.Addr{first, second}) {
		t.Fatalf("added %v, %v", added, err)
	}

	added, err = manager.Add([]netip.Addr{first, second})
	if err != nil || len(added) != 0 {
		t.Fatalf("second add must change nothing: %v, %v", added, err)
	}

	if _, err := manager.Close(); err != nil || len(kernel.links) != 0 {
		t.Fatalf("close: %v, links %v", err, kernel.links)
	}
}

func TestManagerKeepsForeignAddresses(t *testing.T) {
	kernel := newFakeKernel()
	kernel.links["cbgp0"] = &fakeLink{addresses: []netip.Addr{first, other}}
	manager := NewManager(kernel, "cbgp0")
	if _, err := manager.Add([]netip.Addr{first, second}); err != nil {
		t.Fatal(err)
	}

	removed, err := manager.Close()
	if err != nil || !slices.Equal(removed, []netip.Addr{second}) {
		t.Fatalf("removed %v, %v", removed, err)
	}

	link, exists := kernel.links["cbgp0"]
	if !exists || !slices.Equal(link.addresses, []netip.Addr{first, other}) {
		t.Fatalf("foreign link changed: %+v", link)
	}
}

func TestManagerRecoversAfterCrash(t *testing.T) {
	kernel := newFakeKernel()
	if _, err := NewManager(kernel, "cbgp0").Add([]netip.Addr{first, second}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(kernel, "cbgp0")
	if _, err := manager.Add([]netip.Addr{second}); err != nil {
		t.Fatal(err)
	}

	removed, err := manager.Prune([]netip.Addr{second})
	if err != nil || !slices.Equal(removed, []netip.Addr{first}) {
		t.Fatalf("removed %v, %v", removed, err)
	}
}

func TestManagerReportsFailure(t *testing.T) {
	kernel := newFakeKernel()
	kernel.refuse = second
	manager := NewManager(kernel, "cbgp0")
	added, err := manager.Add([]netip.Addr{first, second})
	if err == nil || !slices.Equal(added, []netip.Addr{first}) {
		t.Fatalf("added %v, %v", added, err)
	}
}
