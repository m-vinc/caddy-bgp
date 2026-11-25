package speaker

import (
	"context"
	"net/netip"
	"slices"
	"strings"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

func stateName(state bgp.FSMState) string {
	name := strings.ToLower(state.String())
	return strings.TrimPrefix(name, "bgp_fsm_")
}

func reasonName(reason api.PeerState_DisconnectReason) string {
	return strings.ToLower(strings.TrimPrefix(reason.String(), "DISCONNECT_REASON_"))
}

func sessionName(state api.PeerState_SessionState) string {
	return strings.ToLower(strings.TrimPrefix(state.String(), "SESSION_STATE_"))
}

func (s *Speaker) Status() []PeerStatus {
	if s.bgp == nil {
		return []PeerStatus{}
	}

	result := make([]PeerStatus, 0, len(s.peers))
	err := s.bgp.ListPeer(context.Background(), &api.ListPeerRequest{}, func(peer *api.Peer) {
		result = append(result, s.status(peer))
	})
	if err != nil {
		s.logger.Error("could not list the bgp peers", "error", err)
		return []PeerStatus{}
	}

	slices.SortFunc(result, func(a, b PeerStatus) int { return strings.Compare(a.Address, b.Address) })
	return result
}

func (s *Speaker) status(peer *api.Peer) PeerStatus {
	state := peer.GetState()
	status := PeerStatus{
		Address:      state.GetNeighborAddress(),
		RemoteAS:     peer.GetConf().GetPeerAsn(),
		LocalAddress: peer.GetTransport().GetLocalAddress(),
		State:        sessionName(state.GetSessionState()),
		Established:  state.GetSessionState() == api.PeerState_SESSION_STATE_ESTABLISHED,
	}
	for _, family := range peer.GetAfiSafis() {
		status.ReceivedRoutes += family.GetState().GetReceived()
	}

	if peer.GetBfd().GetEnabled() {
		status.BFD = strings.ToLower(strings.TrimPrefix(state.GetBfdState().GetSessionState().String(), "BFD_SESSION_STATE_"))
	}

	if address, err := netip.ParseAddr(status.Address); err == nil {
		record := s.lookup(address)
		status.Flaps = record.flaps
		status.LastDown = record.lastDown
	}

	if !status.Established {
		return status
	}

	status.RouterID = state.GetRouterId()
	status.HoldTime = peer.GetTimers().GetState().GetNegotiatedHoldTime()
	if uptime := peer.GetTimers().GetState().GetUptime(); uptime != nil {
		since := uptime.AsTime()
		status.Since = &since
	}

	status.Advertised = s.advertised(status.Address)
	status.ImportedRoutes = s.imported(status.Address)
	return status
}

func (s *Speaker) imported(address string) uint64 {
	count := uint64(0)
	for _, family := range []bgp.Family{bgp.RF_IPv4_UC, bgp.RF_IPv6_UC} {
		request := apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: family}
		err := s.bgp.ListPath(request, func(_ bgp.NLRI, paths []*apiutil.Path) {
			count += pathsFrom(paths, address)
		})
		if err != nil {
			s.logger.Debug("could not list the local table", "error", err)
		}
	}

	return count
}

func pathsFrom(paths []*apiutil.Path, address string) uint64 {
	count := uint64(0)
	for _, path := range paths {
		if path.PeerAddress.String() == address {
			count++
		}
	}

	return count
}

func (s *Speaker) advertised(address string) []string {
	var prefixes []string

	for _, family := range []bgp.Family{bgp.RF_IPv4_UC, bgp.RF_IPv6_UC} {
		request := apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_ADJ_OUT, Name: address, Family: family}
		err := s.bgp.ListPath(request, func(prefix bgp.NLRI, _ []*apiutil.Path) {
			prefixes = append(prefixes, prefix.String())
		})
		if err != nil {
			s.logger.Debug("could not list the advertised routes", "peer", address, "error", err)
		}
	}

	slices.Sort(prefixes)
	return prefixes
}
