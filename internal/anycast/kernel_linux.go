//go:build linux

package anycast

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

const linkAlias = "managed by caddy-bgp"

type netlinkKernel struct{}

func SystemKernel() Kernel {
	return netlinkKernel{}
}

func (k netlinkKernel) EnsureLink(name string) (LinkState, error) {
	link, err := netlink.LinkByName(name)
	if _, missing := errors.AsType[netlink.LinkNotFoundError](err); missing {
		link, err = k.createLink(name)
	}

	if err != nil {
		return LinkState{}, err
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return LinkState{}, fmt.Errorf("set up: %w", err)
	}

	addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return LinkState{}, fmt.Errorf("list addresses: %w", err)
	}

	state := LinkState{Managed: link.Attrs().Alias == linkAlias}
	for _, entry := range addresses {
		address, ok := netip.AddrFromSlice(entry.IP)
		ones, bits := entry.Mask.Size()
		if ok && ones == bits {
			state.Addresses = append(state.Addresses, address.Unmap())
		}
	}

	return state, nil
}

func (k netlinkKernel) createLink(name string) (netlink.Link, error) {
	attributes := netlink.NewLinkAttrs()
	attributes.Name = name
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: attributes}); err != nil {
		return nil, fmt.Errorf("create dummy: %w", err)
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, err
	}

	if err := netlink.LinkSetAlias(link, linkAlias); err != nil {
		return nil, errors.Join(fmt.Errorf("set alias: %w", err), netlink.LinkDel(link))
	}

	if err := netlink.LinkSetIP6AddrGenMode(link, nl.IN6_ADDR_GEN_MODE_NONE); err != nil && !errors.Is(err, unix.EAFNOSUPPORT) {
		return nil, errors.Join(fmt.Errorf("disable link-local address: %w", err), netlink.LinkDel(link))
	}

	return netlink.LinkByName(name)
}

func hostAddress(address netip.Addr) *netlink.Addr {
	mask := net.CIDRMask(address.BitLen(), address.BitLen())
	result := &netlink.Addr{IPNet: &net.IPNet{IP: address.AsSlice(), Mask: mask}}
	if address.Is6() {
		result.Flags = unix.IFA_F_NODAD
	}

	return result
}

func (k netlinkKernel) AddAddress(name string, address netip.Addr) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}

	return netlink.AddrAdd(link, hostAddress(address))
}

func (k netlinkKernel) DeleteAddress(name string, address netip.Addr) error {
	link, err := netlink.LinkByName(name)
	if _, missing := errors.AsType[netlink.LinkNotFoundError](err); missing {
		return nil
	}

	if err != nil {
		return err
	}

	if err := netlink.AddrDel(link, hostAddress(address)); err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) {
		return err
	}

	return nil
}

func (k netlinkKernel) DeleteLink(name string) error {
	link, err := netlink.LinkByName(name)
	if _, missing := errors.AsType[netlink.LinkNotFoundError](err); missing {
		return nil
	}

	if err != nil {
		return err
	}

	return netlink.LinkDel(link)
}
