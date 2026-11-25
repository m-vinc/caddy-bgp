package caddybgp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

func probeHTTP(t *testing.T, probe *HTTPProbe) error {
	t.Helper()

	must(t, probe.Provision(caddy.Context{}))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	return probe.Probe(ctx)
}

type probeCase struct {
	status []string
	code   int
	ok     bool
}

func TestHTTPProbeStatus(t *testing.T) {
	cases := []probeCase{
		{nil, 200, true},
		{nil, 204, true},
		{nil, 302, false},
		{nil, 503, false},
		{[]string{"200"}, 204, false},
		{[]string{"200", "3xx"}, 302, true},
		{[]string{"503"}, 503, true},
	}
	for _, item := range cases {
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://192.0.2.1/")
			w.WriteHeader(item.code)
		}))
		err := probeHTTP(t, &HTTPProbe{URL: Value(target.URL), Status: item.status})
		target.Close()
		if (err == nil) != item.ok {
			t.Errorf("%+v: %v", item, err)
		}
	}
}

func TestHTTPProbeSendsHostAndHeaders(t *testing.T) {
	seen := make(chan *http.Request, 1)
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen <- r }))
	defer target.Close()

	probe := &HTTPProbe{
		URL: Value(target.URL + "/ready"), Method: http.MethodHead, Host: "app.example.com",
		Headers: http.Header{"x-probe": {"bgp"}},
	}
	must(t, probeHTTP(t, probe))
	request := <-seen
	if request.Host != "app.example.com" || request.Method != http.MethodHead || request.Header.Get("X-Probe") != "bgp" || request.URL.Path != "/ready" {
		t.Errorf("got %s %s host %s headers %v", request.Method, request.URL, request.Host, request.Header)
	}
}

func TestHTTPProbeTLS(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer target.Close()

	if err := probeHTTP(t, &HTTPProbe{URL: Value(target.URL)}); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("an unknown certificate was accepted: %v", err)
	}

	must(t, probeHTTP(t, &HTTPProbe{URL: Value(target.URL), TLSInsecure: true}))
}

func TestHTTPProbeTimesOut(t *testing.T) {
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer target.Close()
	defer close(release)

	probe := &HTTPProbe{URL: Value(target.URL)}
	must(t, probe.Provision(caddy.Context{}))
	check := &Check{Timeout: caddy.Duration(100 * time.Millisecond), probe: probe}
	if err := check.run(t.Context()); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("got %v", err)
	}
}

func TestHTTPProbeRejectsBadSettings(t *testing.T) {
	probes := []*HTTPProbe{
		{URL: "ftp://127.0.0.1"},
		{URL: "/healthz"},
		{URL: "http://127.0.0.1", Status: []string{"fine"}},
		{URL: "http://127.0.0.1", Status: []string{"9xx"}},
		{URL: "https://127.0.0.1", TLSRootCA: "testdata/absent.pem"},
		{URL: "https://127.0.0.1", TLSRootCA: "testdata/full.json"},
	}
	for _, probe := range probes {
		if err := probe.Provision(caddy.Context{}); err == nil {
			t.Errorf("%+v was accepted", probe)
		}
	}
}

func probeNet(t *testing.T, probe interface {
	Prober
	Provision(caddy.Context) error
}) error {
	t.Helper()

	must(t, probe.Provision(caddy.Context{}))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	return probe.Probe(ctx)
}

func TestTCPProbe(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	address := listener.Addr().String()
	if err := probeNet(t, &TCPProbe{Address: Value(address)}); err != nil {
		t.Errorf("open port: %v", err)
	}

	must(t, listener.Close())
	if err := probeNet(t, &TCPProbe{Address: Value(address)}); err == nil {
		t.Error("a closed port passed")
	}

	if err := (&TCPProbe{Address: "127.0.0.1"}).Provision(caddy.Context{}); err == nil {
		t.Error("an address without a port was accepted")
	}
}

func TestICMPProbe(t *testing.T) {
	err := probeNet(t, &ICMPProbe{Address: "127.0.0.1"})
	if errors.Is(err, os.ErrPermission) {
		t.Skip("no ping or raw socket here")
	}

	if err != nil {
		t.Errorf("loopback: %v", err)
	}

	if err := probeNet(t, &ICMPProbe{Address: "192.0.2.1"}); err == nil {
		t.Error("an unreachable address answered")
	}

	if err := (&ICMPProbe{Address: "host.example"}).Provision(caddy.Context{}); err == nil {
		t.Error("a name was accepted")
	}
}
