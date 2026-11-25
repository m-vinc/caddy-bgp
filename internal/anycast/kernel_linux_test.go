//go:build linux && netns

package anycast

import (
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"
)

const testLink = "bgptest0"

func linkAddresses(t *testing.T) map[netip.Addr]bool {
	t.Helper()

	state, err := SystemKernel().EnsureLink(testLink)
	if err != nil {
		t.Fatal(err)
	}

	result := make(map[netip.Addr]bool)
	for _, address := range state.Addresses {
		result[address] = true
	}

	return result
}

func removeTestLink(t *testing.T) {
	t.Helper()

	if link, err := netlink.LinkByName(testLink); err == nil {
		if err := netlink.LinkDel(link); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKernelCreatesDummy(t *testing.T) {
	removeTestLink(t)
	defer removeTestLink(t)

	manager := NewManager(SystemKernel(), testLink)
	for range 2 {
		if _, err := manager.Add([]netip.Addr{first, second}); err != nil {
			t.Fatal(err)
		}
	}

	link, err := netlink.LinkByName(testLink)
	if err != nil || link.Type() != "dummy" {
		t.Fatalf("link %v, %v", link, err)
	}

	entries, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil || len(entries) != 2 {
		t.Fatalf("addresses %v, %v", entries, err)
	}

	restarted := NewManager(SystemKernel(), testLink)
	if _, err := restarted.Add([]netip.Addr{first}); err != nil {
		t.Fatal(err)
	}

	removed, err := restarted.Prune([]netip.Addr{first})
	if err != nil || len(removed) != 1 || removed[0] != second {
		t.Fatalf("after a crash, removed %v, %v", removed, err)
	}

	if _, err := restarted.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := netlink.LinkByName(testLink); err == nil {
		t.Fatal("the link it created is still there")
	}
}

func TestKernelKeepsForeignLink(t *testing.T) {
	removeTestLink(t)
	defer removeTestLink(t)

	attributes := netlink.NewLinkAttrs()
	attributes.Name = testLink
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: attributes}); err != nil {
		t.Fatal(err)
	}

	link, err := netlink.LinkByName(testLink)
	if err != nil {
		t.Fatal(err)
	}

	if err := netlink.AddrAdd(link, hostAddress(first)); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(SystemKernel(), testLink)
	if _, err := manager.Add([]netip.Addr{first, second}); err != nil {
		t.Fatal(err)
	}

	if addresses := linkAddresses(t); !addresses[first] || !addresses[second] {
		t.Fatalf("addresses %v", addresses)
	}

	if _, err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	if addresses := linkAddresses(t); !addresses[first] || addresses[second] {
		t.Fatalf("addresses %v", addresses)
	}
}
