package speaker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

type PeerEvent struct {
	Address     netip.Addr
	State       string
	Established bool
	Reason      string
	Detail      string
}

type PeerStatus struct {
	Address        string     `json:"address"`
	RemoteAS       uint32     `json:"remote_as"`
	LocalAddress   string     `json:"local_address,omitempty"`
	State          string     `json:"state"`
	Established    bool       `json:"established"`
	Since          *time.Time `json:"since,omitempty"`
	RouterID       string     `json:"router_id,omitempty"`
	HoldTime       uint64     `json:"negotiated_hold_time,omitempty"`
	Advertised     []string   `json:"advertised,omitempty"`
	ReceivedRoutes uint64     `json:"received_routes"`
	ImportedRoutes uint64     `json:"imported_routes"`
	Flaps          uint64     `json:"flaps"`
	LastDown       string     `json:"last_down_reason,omitempty"`
	BFD            string     `json:"bfd_state,omitempty"`
}

type peerRecord struct {
	established bool
	flaps       uint64
	lastDown    string
}

type Speaker struct {
	logger  *slog.Logger
	onEvent func(PeerEvent)

	bgp    *server.BgpServer
	cancel context.CancelFunc
	global Global
	peers  map[netip.Addr]Peer
	routes map[netip.Prefix]Route

	recordsMu sync.Mutex
	records   map[netip.Addr]*peerRecord
}

func New(logger *slog.Logger, onEvent func(PeerEvent)) *Speaker {
	return &Speaker{
		logger:  logger,
		onEvent: onEvent,
		peers:   make(map[netip.Addr]Peer),
		routes:  make(map[netip.Prefix]Route),
		records: make(map[netip.Addr]*peerRecord),
	}
}

func (s *Speaker) Apply(global Global, peers []Peer) (bool, error) {
	restarted := false
	if s.bgp != nil && !s.global.Equal(global) {
		s.Stop()
		restarted = true
	}

	if s.bgp == nil {
		if err := s.start(global); err != nil {
			return restarted, err
		}
	}

	return restarted, s.applyPeers(peers)
}

func (s *Speaker) start(global Global) error {
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	instance := server.NewBgpServer(server.LoggerOption(s.logger, level))
	go instance.Serve()

	ctx, cancel := context.WithCancel(context.Background())
	if err := s.configure(ctx, instance, global); err != nil {
		cancel()
		instance.Stop()
		return err
	}

	s.bgp = instance
	s.cancel = cancel
	s.global = global
	return nil
}

func (s *Speaker) configure(ctx context.Context, instance *server.BgpServer, global Global) error {
	request := &api.Global{Asn: global.LocalAS, RouterId: global.RouterID.String(), ListenPort: -1}
	if len(global.Listen) > 0 {
		request.ListenPort = int32(global.Port)
		for _, address := range global.Listen {
			request.ListenAddresses = append(request.ListenAddresses, address.String())
		}
	}

	if err := instance.StartBgp(ctx, &api.StartBgpRequest{Global: request}); err != nil {
		return fmt.Errorf("start bgp: %w", err)
	}

	if err := installPolicies(ctx, instance); err != nil {
		return err
	}

	callbacks := server.WatchEventMessageCallbacks{OnPeerUpdate: s.handlePeerUpdate}
	if err := instance.WatchEvent(ctx, callbacks, server.WatchPeer()); err != nil {
		return fmt.Errorf("watch peers: %w", err)
	}

	return nil
}

func (s *Speaker) applyPeers(peers []Peer) error {
	wanted := make(map[netip.Addr]Peer, len(peers))
	for _, peer := range peers {
		wanted[peer.Address] = peer
	}

	for address := range s.peers {
		if _, keep := wanted[address]; keep {
			continue
		}

		if err := s.deletePeer(address); err != nil {
			return err
		}
	}

	for _, peer := range peers {
		if err := s.applyPeer(peer); err != nil {
			return err
		}
	}

	return nil
}

func (s *Speaker) applyPeer(peer Peer) error {
	current, exists := s.peers[peer.Address]
	if exists && current == peer {
		return nil
	}

	if !exists {
		if err := s.bgp.AddPeer(context.Background(), &api.AddPeerRequest{Peer: peer.api()}); err != nil {
			return fmt.Errorf("add peer %s: %w", peer.Address, err)
		}

		s.peers[peer.Address] = peer
		return nil
	}

	if _, err := s.bgp.UpdatePeer(context.Background(), &api.UpdatePeerRequest{Peer: peer.api()}); err != nil {
		return fmt.Errorf("update peer %s: %w", peer.Address, errors.Join(err, s.restorePeer(current)))
	}

	s.peers[peer.Address] = peer
	return nil
}

func (s *Speaker) restorePeer(peer Peer) error {
	if s.peerExists(peer.Address) {
		return nil
	}

	if err := s.bgp.AddPeer(context.Background(), &api.AddPeerRequest{Peer: peer.api()}); err != nil {
		delete(s.peers, peer.Address)
		return fmt.Errorf("restore peer: %w", err)
	}

	return nil
}

func (s *Speaker) peerExists(address netip.Addr) bool {
	found := false
	err := s.bgp.ListPeer(context.Background(), &api.ListPeerRequest{Address: address.String()}, func(*api.Peer) { found = true })
	return err == nil && found
}

func (s *Speaker) deletePeer(address netip.Addr) error {
	if err := s.bgp.DeletePeer(context.Background(), &api.DeletePeerRequest{Address: address.String()}); err != nil {
		return fmt.Errorf("delete peer %s: %w", address, err)
	}

	delete(s.peers, address)
	s.recordsMu.Lock()
	delete(s.records, address)
	s.recordsMu.Unlock()
	return nil
}

func (s *Speaker) Announce(route Route) (bool, error) {
	if s.bgp == nil {
		return false, errors.New("bgp speaker is not running")
	}

	current, exists := s.routes[route.Prefix]
	if exists && current.Equal(route) {
		return false, nil
	}

	path, err := route.path(s.global.LocalAS)
	if err != nil {
		return false, err
	}

	if !exists {
		if err := allowPrefix(context.Background(), s.bgp, route.Prefix); err != nil {
			return false, fmt.Errorf("allow %s: %w", route.Prefix, err)
		}
	}

	if _, err := s.bgp.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{path}}); err != nil {
		return false, fmt.Errorf("announce %s: %w", route.Prefix, err)
	}

	s.routes[route.Prefix] = route
	return true, nil
}

func (s *Speaker) Withdraw(prefix netip.Prefix) (bool, error) {
	route, exists := s.routes[prefix]
	if !exists || s.bgp == nil {
		return false, nil
	}

	path, err := route.path(s.global.LocalAS)
	if err != nil {
		return false, err
	}

	if err := s.bgp.DeletePath(apiutil.DeletePathRequest{Paths: []*apiutil.Path{path}}); err != nil {
		return false, fmt.Errorf("withdraw %s: %w", prefix, err)
	}

	delete(s.routes, prefix)
	if err := denyPrefix(context.Background(), s.bgp, prefix); err != nil {
		return true, fmt.Errorf("deny %s: %w", prefix, err)
	}

	return true, nil
}

func (s *Speaker) Announced(prefix netip.Prefix) bool {
	_, exists := s.routes[prefix]
	return exists
}

func (s *Speaker) Prefixes() []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(s.routes))
	for prefix := range s.routes {
		prefixes = append(prefixes, prefix)
	}

	slices.SortFunc(prefixes, netip.Prefix.Compare)
	return prefixes
}

func (s *Speaker) EstablishedPeers() int {
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()

	count := 0
	for _, record := range s.records {
		if record.established {
			count++
		}
	}

	return count
}

func (s *Speaker) Stop() {
	if s.bgp == nil {
		return
	}

	s.bgp.Stop()
	s.cancel()
	s.bgp = nil
	s.peers = make(map[netip.Addr]Peer)
	s.routes = make(map[netip.Prefix]Route)
	s.recordsMu.Lock()
	s.records = make(map[netip.Addr]*peerRecord)
	s.recordsMu.Unlock()
}

func (s *Speaker) handlePeerUpdate(event *apiutil.WatchEventMessage_PeerEvent, _ time.Time) {
	if event.Type != apiutil.PEER_EVENT_STATE {
		return
	}

	state := event.Peer.State
	established := state.SessionState == bgp.BGP_FSM_ESTABLISHED
	result := PeerEvent{
		Address:     state.NeighborAddress,
		State:       stateName(state.SessionState),
		Established: established,
	}
	if state.DisconnectReason != api.PeerState_DISCONNECT_REASON_UNSPECIFIED {
		result.Reason = reasonName(state.DisconnectReason)
		result.Detail = state.DisconnectMessage
	}

	if s.record(result) && s.onEvent != nil {
		s.onEvent(result)
	}
}

func (s *Speaker) record(event PeerEvent) bool {
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()

	current, exists := s.records[event.Address]
	if !exists {
		current = &peerRecord{}
		s.records[event.Address] = current
	}

	if current.established == event.Established {
		return false
	}

	current.established = event.Established
	if !event.Established {
		current.flaps++
		current.lastDown = event.Reason
	}

	return true
}

func (s *Speaker) lookup(address netip.Addr) peerRecord {
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()

	if record, exists := s.records[address]; exists {
		return *record
	}

	return peerRecord{}
}
