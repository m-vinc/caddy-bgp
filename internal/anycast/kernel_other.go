//go:build !linux

package anycast

import (
	"errors"
	"net/netip"
)

var errUnsupported = errors.New("managing addresses is only supported on linux")

type unsupportedKernel struct{}

func SystemKernel() Kernel {
	return unsupportedKernel{}
}

func (unsupportedKernel) EnsureLink(string) (LinkState, error) {
	return LinkState{}, errUnsupported
}

func (unsupportedKernel) AddAddress(string, netip.Addr) error {
	return errUnsupported
}

func (unsupportedKernel) DeleteAddress(string, netip.Addr) error {
	return errUnsupported
}

func (unsupportedKernel) DeleteLink(string) error {
	return errUnsupported
}
