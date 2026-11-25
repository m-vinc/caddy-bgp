package speaker

import (
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

type Global struct {
	LocalAS  uint32
	RouterID netip.Addr
	Listen   []netip.Addr
	Port     uint16
}

func (g Global) Equal(other Global) bool {
	return g.LocalAS == other.LocalAS && g.RouterID == other.RouterID && g.Port == other.Port && slices.Equal(g.Listen, other.Listen)
}

type BFD struct {
	Interval   time.Duration
	Multiplier uint32
	Multihop   bool
}

type Peer struct {
	Address      netip.Addr
	RemoteAS     uint32
	LocalAddress netip.Addr
	Port         uint16
	IPv4         bool
	IPv6         bool
	Password     string
	Passive      bool
	MultihopTTL  uint8
	TTLHops      uint8
	HoldTime     time.Duration
	Keepalive    time.Duration
	ConnectRetry time.Duration
	MaxPrefixes  uint32
	BFD          BFD
	BFDEnabled   bool
}

type Route struct {
	Prefix           netip.Prefix
	NextHop          netip.Addr
	Communities      []uint32
	LargeCommunities []string
	MED              uint32
	HasMED           bool
	Prepend          uint8
}

func (r Route) Equal(other Route) bool {
	return r.Prefix == other.Prefix && r.NextHop == other.NextHop && r.MED == other.MED && r.HasMED == other.HasMED &&
		r.Prepend == other.Prepend && slices.Equal(r.Communities, other.Communities) &&
		slices.Equal(r.LargeCommunities, other.LargeCommunities)
}

func (p Peer) api() *api.Peer {
	peer := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress: p.Address.String(),
			PeerAsn:         p.RemoteAS,
			AuthPassword:    p.Password,
		},
		Transport: &api.Transport{
			PassiveMode: p.Passive,
			RemotePort:  uint32(p.Port),
		},
		Timers: &api.Timers{Config: &api.TimersConfig{
			HoldTime:          uint64(p.HoldTime / time.Second),
			KeepaliveInterval: uint64(p.Keepalive / time.Second),
			ConnectRetry:      uint64(p.ConnectRetry / time.Second),
		}},
	}
	if p.LocalAddress.IsValid() {
		peer.Transport.LocalAddress = p.LocalAddress.String()
	}

	if p.IPv4 {
		peer.AfiSafis = append(peer.AfiSafis, p.family(api.Family_AFI_IP))
	}

	if p.IPv6 {
		peer.AfiSafis = append(peer.AfiSafis, p.family(api.Family_AFI_IP6))
	}

	if p.MultihopTTL > 0 {
		peer.EbgpMultihop = &api.EbgpMultihop{Enabled: true, MultihopTtl: uint32(p.MultihopTTL)}
	}

	if p.TTLHops > 0 {
		peer.TtlSecurity = &api.TtlSecurity{Enabled: true, TtlMin: 256 - uint32(p.TTLHops)}
	}

	if p.BFDEnabled {
		interval := uint32(p.BFD.Interval / time.Microsecond)
		peer.Bfd = &api.BfdPeerConfig{
			Enabled:                  true,
			DesiredMinimumTxInterval: interval,
			RequiredMinimumReceive:   interval,
			DetectionMultiplier:      p.BFD.Multiplier,
			Multihop:                 p.BFD.Multihop,
		}
	}

	return peer
}

func (p Peer) family(afi api.Family_Afi) *api.AfiSafi {
	family := &api.Family{Afi: afi, Safi: api.Family_SAFI_UNICAST}
	result := &api.AfiSafi{Config: &api.AfiSafiConfig{Family: family, Enabled: true}}
	if p.MaxPrefixes > 0 {
		result.PrefixLimits = &api.PrefixLimit{Family: family, MaxPrefixes: p.MaxPrefixes, ShutdownThresholdPct: 100}
	}

	return result
}

func family(prefix netip.Prefix) bgp.Family {
	if prefix.Addr().Is4() {
		return bgp.RF_IPv4_UC
	}

	return bgp.RF_IPv6_UC
}

func (r Route) path(localAS uint32) (*apiutil.Path, error) {
	nlri, err := bgp.NewIPAddrPrefix(r.Prefix)
	if err != nil {
		return nil, fmt.Errorf("prefix %s: %w", r.Prefix, err)
	}

	nextHop := r.NextHop
	if !nextHop.IsValid() {
		nextHop = netip.IPv4Unspecified()
		if r.Prefix.Addr().Is6() {
			nextHop = netip.IPv6Unspecified()
		}
	}

	attributes := []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(bgp.BGP_ORIGIN_ATTR_TYPE_IGP)}
	reach, err := nextHopAttribute(r.Prefix, nlri, nextHop)
	if err != nil {
		return nil, fmt.Errorf("next hop %s: %w", nextHop, err)
	}

	attributes = append(attributes, reach)
	if r.Prepend > 0 {
		attributes = append(attributes, prependAttribute(localAS, r.Prepend))
	}

	if r.HasMED {
		attributes = append(attributes, bgp.NewPathAttributeMultiExitDisc(r.MED))
	}

	if len(r.Communities) > 0 {
		attributes = append(attributes, bgp.NewPathAttributeCommunities(r.Communities))
	}

	if len(r.LargeCommunities) > 0 {
		large, err := largeCommunities(r.LargeCommunities)
		if err != nil {
			return nil, err
		}

		attributes = append(attributes, large)
	}

	return &apiutil.Path{Family: family(r.Prefix), Nlri: nlri, Attrs: attributes}, nil
}

func nextHopAttribute(prefix netip.Prefix, nlri bgp.NLRI, nextHop netip.Addr) (bgp.PathAttributeInterface, error) {
	if prefix.Addr().Is4() {
		return bgp.NewPathAttributeNextHop(nextHop)
	}

	return bgp.NewPathAttributeMpReachNLRI(bgp.RF_IPv6_UC, []bgp.PathNLRI{{NLRI: nlri}}, nextHop)
}

func prependAttribute(localAS uint32, count uint8) bgp.PathAttributeInterface {
	sequence := slices.Repeat([]uint32{localAS}, int(count))
	return bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{
		bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, sequence),
	})
}

func largeCommunities(values []string) (bgp.PathAttributeInterface, error) {
	parsed := make([]*bgp.LargeCommunity, 0, len(values))
	for _, value := range values {
		community, err := bgp.ParseLargeCommunity(value)
		if err != nil {
			return nil, fmt.Errorf("large community %s: %w", value, err)
		}

		parsed = append(parsed, community)
	}

	return bgp.NewPathAttributeLargeCommunities(parsed), nil
}
