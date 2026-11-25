package caddybgp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

func init() {
	caddy.RegisterModule(ServerProbe{})
	caddy.RegisterModule(UpstreamsProbe{})
}

type ServerProbe struct {
	Name string `json:"name"`

	dials []caddy.NetworkAddress
}

func (ServerProbe) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "bgp.probes.server", New: func() caddy.Module { return new(ServerProbe) }}
}

func httpServer(ctx caddy.Context, name string) (*caddyhttp.Server, error) {
	app, err := httpApp(ctx)
	if err != nil {
		return nil, err
	}

	server, exists := app.Servers[name]
	if !exists {
		names := make([]string, 0, len(app.Servers))
		for known := range app.Servers {
			names = append(names, known)
		}

		slices.Sort(names)
		return nil, fmt.Errorf("no http server is named %q, the servers are: %s", name, strings.Join(names, ", "))
	}

	return server, nil
}

func httpApp(ctx caddy.Context) (*caddyhttp.App, error) {
	loaded, err := ctx.AppIfConfigured("http")
	if err != nil {
		return nil, fmt.Errorf("the http app is needed by this check: %w", err)
	}

	app, ok := loaded.(*caddyhttp.App)
	if !ok {
		return nil, fmt.Errorf("unexpected http app %T", loaded)
	}

	return app, nil
}

func (p *ServerProbe) Provision(ctx caddy.Context) error {
	server, err := httpServer(ctx, p.Name)
	if err != nil {
		return err
	}

	for _, listen := range server.Listen {
		address, err := caddy.ParseNetworkAddress(listen)
		if err != nil {
			return fmt.Errorf("listen address %s: %w", listen, err)
		}

		if address.IsUnixNetwork() || strings.HasPrefix(address.Network, "tcp") {
			p.dials = append(p.dials, address)
		}
	}

	if len(p.dials) == 0 {
		return fmt.Errorf("server %s has no TCP or unix listen address", p.Name)
	}

	return nil
}

func (p *ServerProbe) Probe(ctx context.Context) error {
	for _, address := range p.dials {
		if err := dialPorts(ctx, address); err != nil {
			return err
		}
	}

	return nil
}

func dialPorts(ctx context.Context, address caddy.NetworkAddress) error {
	if address.IsUnixNetwork() {
		return dial(ctx, "unix", address.Host)
	}

	host := address.Host
	if parsed := net.ParseIP(host); host == "" || (parsed != nil && parsed.IsUnspecified()) {
		host = "127.0.0.1"
		if (parsed != nil && parsed.To4() == nil) || address.Network == "tcp6" {
			host = "::1"
		}
	}

	for port := address.StartPort; port <= address.EndPort; port++ {
		if err := dial(ctx, "tcp", net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10))); err != nil {
			return err
		}
	}

	return nil
}

func dial(ctx context.Context, network, address string) error {
	var dialer net.Dialer

	connection, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return err
	}

	return connection.Close()
}

func (p *ServerProbe) String() string {
	return "server " + p.Name
}

func (p *ServerProbe) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if !d.AllArgs(&p.Name) {
		return d.ArgErr()
	}

	if d.NextBlock(d.Nesting()) {
		return d.Errf("unknown server check option %q", d.Val())
	}

	return nil
}

type UpstreamsProbe struct {
	Server       string   `json:"server,omitempty"`
	Dials        []string `json:"dials,omitempty"`
	MinAvailable int      `json:"min_available,omitempty"`

	upstreams map[string][]*reverseproxy.Upstream
}

func (UpstreamsProbe) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "bgp.probes.upstreams", New: func() caddy.Module { return new(UpstreamsProbe) }}
}

func (p *UpstreamsProbe) Provision(ctx caddy.Context) error {
	servers, err := p.servers(ctx)
	if err != nil {
		return err
	}

	if p.MinAvailable == 0 {
		p.MinAvailable = 1
	}

	p.upstreams = make(map[string][]*reverseproxy.Upstream)
	for _, server := range servers {
		p.collect(server.Routes)
		for _, route := range server.NamedRoutes {
			p.collect(caddyhttp.RouteList{*route})
		}
	}

	if p.MinAvailable < 0 || len(p.upstreams) < p.MinAvailable {
		return fmt.Errorf("%d static upstreams match, min_available is %d", len(p.upstreams), p.MinAvailable)
	}

	return nil
}

func (p *UpstreamsProbe) servers(ctx caddy.Context) (map[string]*caddyhttp.Server, error) {
	if p.Server == "" {
		app, err := httpApp(ctx)
		if err != nil {
			return nil, err
		}

		return app.Servers, nil
	}

	server, err := httpServer(ctx, p.Server)
	if err != nil {
		return nil, err
	}

	return map[string]*caddyhttp.Server{p.Server: server}, nil
}

func (p *UpstreamsProbe) collect(routes caddyhttp.RouteList) {
	for _, route := range routes {
		for _, handler := range route.Handlers {
			switch typed := handler.(type) {
			case *caddyhttp.Subroute:
				p.collect(typed.Routes)
			case *reverseproxy.Handler:
				p.add(typed.Upstreams)
			}
		}
	}
}

func (p *UpstreamsProbe) add(pool reverseproxy.UpstreamPool) {
	for _, upstream := range pool {
		if len(p.Dials) == 0 || slices.Contains(p.Dials, upstream.Dial) {
			p.upstreams[upstream.Dial] = append(p.upstreams[upstream.Dial], upstream)
		}
	}
}

func (p *UpstreamsProbe) Probe(context.Context) error {
	var down []string

	for dial, upstreams := range p.upstreams {
		if slices.ContainsFunc(upstreams, unhealthy) {
			down = append(down, dial)
		}
	}

	if len(p.upstreams)-len(down) < p.MinAvailable {
		slices.Sort(down)
		return errors.New("upstreams down: " + strings.Join(down, ", "))
	}

	return nil
}

func unhealthy(upstream *reverseproxy.Upstream) bool {
	return !upstream.Healthy()
}

func (p *UpstreamsProbe) String() string {
	parts := []string{"upstreams"}
	if p.Server != "" {
		parts = append(parts, "of "+p.Server)
	}

	return strings.Join(append(parts, p.Dials...), " ")
}

func (p *UpstreamsProbe) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	p.Dials = d.RemainingArgs()
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		option := d.Val()

		var argument string
		if !d.AllArgs(&argument) {
			return d.ArgErr()
		}

		switch option {
		case "server":
			p.Server = argument
		case "min_available":
			number, err := strconv.Atoi(argument)
			if err != nil {
				return d.Errf("min_available: %v", err)
			}

			p.MinAvailable = number
		default:
			return d.Errf("unknown upstreams check option %q", option)
		}
	}

	return nil
}

var (
	_ caddy.Provisioner     = (*ServerProbe)(nil)
	_ caddyfile.Unmarshaler = (*ServerProbe)(nil)
	_ Prober                = (*ServerProbe)(nil)
	_ caddy.Provisioner     = (*UpstreamsProbe)(nil)
	_ caddyfile.Unmarshaler = (*UpstreamsProbe)(nil)
	_ Prober                = (*UpstreamsProbe)(nil)
)
