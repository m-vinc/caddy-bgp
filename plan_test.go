package caddybgp

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

func planOf(t *testing.T, source string) (*plan, error) {
	t.Helper()

	var config Config
	must(t, json.Unmarshal([]byte(source), &config))
	return config.plan(caddy.NewReplacer())
}

const minimal = `{"local_as": 65000, "router_id": "192.0.2.10", "peers": [{"address": "192.0.2.1", "remote_as": 64500}],
	"announcements": [{"name": "web", "prefixes": ["198.51.100.100"], "always": true}]}`

func TestPlanDefaults(t *testing.T) {
	planned, err := planOf(t, minimal)
	must(t, err)
	peer := planned.peers[0]
	if peer.HoldTime != 9*time.Second || peer.Keepalive != 3*time.Second || peer.ConnectRetry != 5*time.Second || !peer.IPv4 || peer.IPv6 {
		t.Errorf("peer %+v", peer)
	}

	if len(planned.global.Listen) != 0 || planned.global.Port != 0 {
		t.Errorf("a listener is configured: %+v", planned.global)
	}

	route := planned.announcements[0].routes[0]
	if route.Prefix.String() != "198.51.100.100/32" || route.NextHop.IsValid() {
		t.Errorf("route %+v", route)
	}

	if planned.drainDelay != 5*time.Second || planned.announcements[0].settings.HoldDown != 10*time.Second {
		t.Errorf("plan %+v", planned)
	}

	if !slices.Equal(planned.addresses, []netip.Addr{netip.MustParseAddr("198.51.100.100")}) {
		t.Errorf("addresses %v", planned.addresses)
	}
}

func TestPlanResolvesPlaceholders(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	must(t, os.WriteFile(secret, []byte("hunter2\n"), 0o600))
	t.Setenv("TEST_ROUTER_ID", "192.0.2.77")
	t.Setenv("TEST_AS", "65010")
	t.Setenv("TEST_PEER", "2001:db8::1")
	t.Setenv("TEST_PEER2", "")
	planned, err := planOf(t, `{"local_as": "{env.TEST_AS}", "router_id": "{env.TEST_ROUTER_ID}", "peers": [
		{"address": "{env.TEST_PEER}", "remote_as": 64500, "password": "{file.`+secret+`}"},
		{"address": "{env.TEST_PEER2}", "remote_as": 64501, "optional": true}]}`)
	must(t, err)
	if planned.global.LocalAS != 65010 || planned.global.RouterID.String() != "192.0.2.77" {
		t.Errorf("global %+v", planned.global)
	}

	if len(planned.peers) != 1 || planned.peers[0].Password != "hunter2" || !planned.peers[0].IPv6 {
		t.Errorf("peers %+v", planned.peers)
	}
}

type badPlan struct {
	replace string
	with    string
	want    string
}

func TestPlanRejects(t *testing.T) {
	cases := map[string]badPlan{
		"zero AS":             {`"local_as": 65000`, `"local_as": 0`, "local_as"},
		"router id v6":        {"192.0.2.10", "2001:db8::1", "router_id"},
		"unset placeholder":   {`"192.0.2.10"`, `"{env.TEST_UNSET_VARIABLE}"`, "router_id"},
		"unknown placeholder": {`"192.0.2.10"`, `"{nonsense.x}"`, "unrecognized placeholder"},
		"bad peer":            {"192.0.2.1\"", "rt1\"", "not an IP address"},
		"passive no listener": {`"remote_as": 64500`, `"remote_as": 64500, "passive": true`, "needs a listen address"},
		"both ttl modes":      {`"remote_as": 64500`, `"remote_as": 64500, "multihop_ttl": 2, "ttl_security_hops": 1`, "mutually exclusive"},
		"short hold time":     {`"remote_as": 64500`, `"remote_as": 64500, "hold_time": "1s"`, "hold_time"},
		"fractional hold":     {`"remote_as": 64500`, `"remote_as": 64500, "hold_time": "3500ms"`, "hold_time"},
		"long keepalive":      {`"remote_as": 64500`, `"remote_as": 64500, "keepalive_interval": "9s"`, "keepalive_interval"},
		"unknown family":      {`"remote_as": 64500`, `"remote_as": 64500, "families": ["evpn"]`, "unknown family"},
		"empty password":      {`"remote_as": 64500`, `"remote_as": 64500, "password": "{env.TEST_UNSET_VARIABLE}"`, "resolves to an empty string"},
		"local address":       {`"remote_as": 64500`, `"remote_as": 64500, "local_address": "2001:db8::1"`, "family of the peer"},
		"bfd interval":        {`"remote_as": 64500`, `"remote_as": 64500, "bfd": {"interval": "1ms"}`, "bfd"},
		"host bits":           {`"198.51.100.100"`, `"198.51.100.100/24"`, "host bits"},
		"default route":       {`"198.51.100.100"`, `"0.0.0.0/0"`, "cannot be announced"},
		"default route v6":    {`"198.51.100.100"`, `"::/0"`, "cannot be announced"},
		"no family":           {`"198.51.100.100"`, `"2001:db8::1"`, "no peer carries its family"},
		"missing next hop":    {`"remote_as": 64500}], `, `"remote_as": 64500, "families": ["ipv6"]}], `, "no peer carries"},
		"duplicate prefix":    {`["198.51.100.100"]`, `["198.51.100.100", "198.51.100.100/32"]`, "is in announcements"},
		"no check":            {`"always": true`, `"always": false`, "no check"},
		"bad community":       {`"always": true`, `"always": true, "communities": ["70000:1"]`, "community"},
		"bad large community": {`"always": true`, `"always": true, "communities": ["1:2:x"]`, "community"},
		"next hop family":     {`"always": true`, `"always": true, "next_hops": ["192.0.2.1", "192.0.2.2"]`, "one address per family"},
		"next hop loopback":   {`"always": true`, `"always": true, "next_hops": ["127.0.0.1"]`, "not usable"},
		"prepend":             {`"always": true`, `"always": true, "as_path_prepend": 99`, "as_path_prepend"},
		"prepend ibgp":        {`"remote_as": 64500}], `, `"remote_as": 65000}], `, ""},
		"negative drain":      {`"local_as": 65000`, `"local_as": 65000, "drain_delay": "-1s"`, "drain_delay"},
		"interface name":      {`"local_as": 65000`, `"local_as": 65000, "interface": "a-name-that-is-too-long"`, "interface"},
		"no peers":            {`{"address": "192.0.2.1", "remote_as": 64500}`, ``, "no peer"},
		"duplicate peer": {`{"address": "192.0.2.1", "remote_as": 64500}`,
			`{"address": "192.0.2.1", "remote_as": 64500}, {"address": "192.0.2.1", "remote_as": 64500}`, "configured twice"},
	}
	compact := strings.ReplaceAll(minimal, "\n\t", " ")
	for name, item := range cases {
		if !strings.Contains(compact, item.replace) {
			t.Fatalf("%s: %q is not in the template", name, item.replace)
		}

		source := strings.Replace(compact, item.replace, item.with, 1)
		_, err := planOf(t, source)
		if item.want == "" {
			must(t, err)
			continue
		}

		if err == nil || !strings.Contains(err.Error(), item.want) {
			t.Errorf("%s: got %v, want %q", name, err, item.want)
		}
	}
}

func TestPlanNeedsNextHopAcrossFamilies(t *testing.T) {
	source := `{"local_as": 65000, "router_id": "192.0.2.10", "peers": [{"address": "192.0.2.1", "remote_as": 64500, "families": ["ipv4", "ipv6"]}],
		"announcements": [{"name": "web", "prefixes": ["2001:db8::1"], "always": true%s}]}`
	if _, err := planOf(t, strings.Replace(source, "%s", "", 1)); err == nil || !strings.Contains(err.Error(), "needs a next_hop") {
		t.Errorf("got %v", err)
	}

	_, err := planOf(t, strings.Replace(source, "%s", `, "next_hops": ["2001:db8::10"]`, 1))
	must(t, err)
}

func TestPlanRejectsPrependToInternalPeer(t *testing.T) {
	source := `{"local_as": 65000, "router_id": "192.0.2.10", "peers": [{"address": "192.0.2.1", "remote_as": 65000}],
		"announcements": [{"name": "web", "prefixes": ["198.51.100.1"], "always": true, "as_path_prepend": 1}]}`
	if _, err := planOf(t, source); err == nil || !strings.Contains(err.Error(), "internal peer") {
		t.Errorf("got %v", err)
	}
}
