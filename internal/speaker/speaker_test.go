package speaker

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

var (
	allowed   = netip.MustParsePrefix("198.51.100.1/32")
	allowed6  = netip.MustParsePrefix("2001:db8::1/128")
	smuggled  = netip.MustParsePrefix("203.0.113.0/24")
	inside    = netip.MustParsePrefix("198.51.100.1/32")
	loopback4 = netip.MustParseAddr("127.0.0.1")
)

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for: %s", what)
}

func newPeerServer(t *testing.T) (*server.BgpServer, uint16) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	must(t, listener.Close())

	instance := server.NewBgpServer()
	go instance.Serve()

	must(t, instance.StartBgp(t.Context(), &api.StartBgpRequest{Global: &api.Global{
		Asn: 64500, RouterId: "203.0.113.1", ListenPort: int32(port), ListenAddresses: []string{"127.0.0.1"},
	}}))
	must(t, instance.AddPeer(t.Context(), &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65000},
		Transport: &api.Transport{PassiveMode: true},
		AfiSafis: []*api.AfiSafi{
			{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}, Enabled: true}},
			{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}, Enabled: true}},
		},
	}}))
	t.Cleanup(instance.Stop)
	return instance, uint16(port)
}

func prefixesOf(t *testing.T, instance *server.BgpServer) []string {
	t.Helper()

	prefixes := []string{}
	for _, family := range []bgp.Family{bgp.RF_IPv4_UC, bgp.RF_IPv6_UC} {
		request := apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: family}
		must(t, instance.ListPath(request, func(prefix bgp.NLRI, _ []*apiutil.Path) {
			prefixes = append(prefixes, prefix.String())
		}))
	}

	return prefixes
}

func newTestSpeaker(t *testing.T, port uint16) *Speaker {
	t.Helper()

	s := New(slog.New(slog.DiscardHandler), nil)
	_, err := s.Apply(
		Global{LocalAS: 65000, RouterID: netip.MustParseAddr("192.0.2.10")},
		[]Peer{{
			Address: loopback4, RemoteAS: 64500, Port: port, IPv4: true, IPv6: true,
			HoldTime: 9 * time.Second, Keepalive: 3 * time.Second, ConnectRetry: 2 * time.Second,
		}},
	)
	must(t, err)
	t.Cleanup(s.Stop)
	return s
}

func TestExportPolicyOnlyLetsAnnouncedPrefixesOut(t *testing.T) {
	remote, port := newPeerServer(t)
	s := newTestSpeaker(t, port)
	for _, prefix := range []netip.Prefix{smuggled, netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("0.0.0.0/0")} {
		path, err := Route{Prefix: prefix}.path(65000)
		must(t, err)
		_, err = s.bgp.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{path}})
		must(t, err)
	}

	_, err := s.Announce(Route{Prefix: allowed})
	must(t, err)
	_, err = s.Announce(Route{Prefix: allowed6, NextHop: netip.MustParseAddr("2001:db8::2")})
	must(t, err)
	eventually(t, "the allowed prefixes", func() bool { return len(prefixesOf(t, remote)) == 2 })
	time.Sleep(500 * time.Millisecond)
	if got := prefixesOf(t, remote); !slices.Equal(got, []string{allowed.String(), allowed6.String()}) {
		t.Fatalf("the peer holds %v", got)
	}

	changed, err := s.Withdraw(inside)
	if err != nil || !changed {
		t.Fatalf("withdraw: %v, %v", changed, err)
	}

	eventually(t, "the withdrawal", func() bool { return slices.Equal(prefixesOf(t, remote), []string{allowed6.String()}) })
	path, err := Route{Prefix: allowed}.path(65000)
	must(t, err)
	_, err = s.bgp.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{path}})
	must(t, err)
	time.Sleep(500 * time.Millisecond)
	if got := prefixesOf(t, remote); !slices.Equal(got, []string{allowed6.String()}) {
		t.Fatalf("a withdrawn prefix must leave the export policy, the peer holds %v", got)
	}
}

func TestAnnounceAndWithdrawAreIdempotent(t *testing.T) {
	_, port := newPeerServer(t)
	s := newTestSpeaker(t, port)
	changed, err := s.Announce(Route{Prefix: allowed})
	if err != nil || !changed {
		t.Fatalf("got %v, %v", changed, err)
	}

	changed, err = s.Announce(Route{Prefix: allowed})
	if err != nil || changed {
		t.Fatalf("got %v, %v", changed, err)
	}

	changed, err = s.Announce(Route{Prefix: allowed, MED: 5, HasMED: true})
	if err != nil || !changed {
		t.Fatalf("got %v, %v", changed, err)
	}

	if changed, err = s.Withdraw(allowed); err != nil || !changed {
		t.Fatalf("got %v, %v", changed, err)
	}

	if changed, err = s.Withdraw(allowed); err != nil || changed {
		t.Fatalf("got %v, %v", changed, err)
	}
}

func TestStopSendsNotificationAndCanStartAgain(t *testing.T) {
	remote, port := newPeerServer(t)
	s := newTestSpeaker(t, port)
	_, err := s.Announce(Route{Prefix: allowed})
	must(t, err)
	eventually(t, "the prefix", func() bool { return len(prefixesOf(t, remote)) == 1 })
	if s.EstablishedPeers() != 1 {
		t.Fatalf("%d peers", s.EstablishedPeers())
	}

	s.Stop()
	if s.bgp != nil || s.EstablishedPeers() != 0 {
		t.Fatal("still running")
	}

	eventually(t, "the routes to leave", func() bool { return len(prefixesOf(t, remote)) == 0 })
	_, err = s.Apply(Global{LocalAS: 65000, RouterID: netip.MustParseAddr("192.0.2.10")}, nil)
	must(t, err)
}

func TestPeerRequest(t *testing.T) {
	peer := Peer{
		Address: netip.MustParseAddr("2001:db8::1"), RemoteAS: 64500, LocalAddress: netip.MustParseAddr("2001:db8::2"),
		IPv6: true, Password: "secret", TTLHops: 1, MaxPrefixes: 10, BFDEnabled: true,
		BFD:      BFD{Interval: 300 * time.Millisecond, Multiplier: 3},
		HoldTime: 9 * time.Second, Keepalive: 3 * time.Second, ConnectRetry: 5 * time.Second,
	}
	request := peer.api()
	if request.GetTtlSecurity().GetTtlMin() != 255 || request.GetEbgpMultihop().GetEnabled() {
		t.Errorf("ttl security %v", request.GetTtlSecurity())
	}

	if request.GetBfd().GetDesiredMinimumTxInterval() != 300000 || request.GetGracefulRestart().GetEnabled() {
		t.Errorf("bfd %v, graceful restart %v", request.GetBfd(), request.GetGracefulRestart())
	}

	if len(request.GetAfiSafis()) != 1 || request.GetAfiSafis()[0].GetPrefixLimits().GetMaxPrefixes() != 10 {
		t.Errorf("families %v", request.GetAfiSafis())
	}

	if request.GetTimers().GetConfig().GetHoldTime() != 9 || request.GetConf().GetAuthPassword() != "secret" {
		t.Errorf("request %v", request)
	}
}

func TestPeerStatusSurvivesUnknownPeer(t *testing.T) {
	_, port := newPeerServer(t)
	s := newTestSpeaker(t, port)
	eventually(t, "the session", func() bool { return s.EstablishedPeers() == 1 })
	status := s.Status()
	if len(status) != 1 || status[0].State != "established" || status[0].HoldTime != 9 || status[0].RouterID != "203.0.113.1" {
		t.Fatalf("status %+v", status)
	}

	must(t, s.bgp.DeletePeer(context.Background(), &api.DeletePeerRequest{Address: "127.0.0.1"}))
	if status := s.Status(); len(status) != 0 {
		t.Fatalf("status %+v", status)
	}
}
