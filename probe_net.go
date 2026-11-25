package caddybgp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

func init() {
	caddy.RegisterModule(TCPProbe{})
	caddy.RegisterModule(ICMPProbe{})
}

const (
	protocolICMP   = 1
	protocolICMPv6 = 58
)

type TCPProbe struct {
	Address Value `json:"address"`

	address string
}

func (TCPProbe) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "bgp.probes.tcp", New: func() caddy.Module { return new(TCPProbe) }}
}

func (p *TCPProbe) Provision(caddy.Context) error {
	address, err := p.Address.resolve(caddy.NewReplacer())
	if err != nil {
		return fmt.Errorf("address: %w", err)
	}

	if _, _, err := net.SplitHostPort(address); err != nil {
		return fmt.Errorf("address %q must be host:port: %w", address, err)
	}

	p.address = address
	return nil
}

func (p *TCPProbe) Probe(ctx context.Context) error {
	return dial(ctx, "tcp", p.address)
}

func (p *TCPProbe) String() string {
	return "tcp " + p.address
}

func (p *TCPProbe) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	return oneProbeArgument(d, "tcp", &p.Address)
}

type ICMPProbe struct {
	Address Value `json:"address"`

	address  netip.Addr
	sequence uint32
}

type icmpSocket struct {
	conn *icmp.PacketConn
	raw  bool
}

func (ICMPProbe) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "bgp.probes.icmp", New: func() caddy.Module { return new(ICMPProbe) }}
}

func (p *ICMPProbe) Provision(caddy.Context) error {
	address, err := resolveAddress(p.Address, caddy.NewReplacer())
	if err != nil {
		return fmt.Errorf("address: %w", err)
	}

	p.address = address
	return nil
}

func (p *ICMPProbe) Probe(ctx context.Context) error {
	socket, err := listenICMP(p.address.Is4())
	if err != nil {
		return err
	}

	defer func() { _ = socket.conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = socket.conn.SetDeadline(time.Now()) })
	defer stop()

	if deadline, ok := ctx.Deadline(); ok {
		if err := socket.conn.SetDeadline(deadline); err != nil {
			return err
		}
	}

	echo := &icmp.Echo{ID: os.Getpid() & 0xffff, Seq: int(atomic.AddUint32(&p.sequence, 1) & 0xffff), Data: []byte("caddy-bgp")}
	request := icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: echo}
	if p.address.Is4() {
		request.Type = ipv4.ICMPTypeEcho
	}

	data, err := request.Marshal(nil)
	if err != nil {
		return err
	}

	if _, err := socket.conn.WriteTo(data, socket.destination(p.address)); err != nil {
		return err
	}

	buffer := make([]byte, 1500)
	for {
		size, peer, err := socket.conn.ReadFrom(buffer)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return fmt.Errorf("no echo reply from %s", p.address)
		}

		if err != nil {
			return err
		}

		if socket.isReply(buffer[:size], peer, p.address, echo) {
			return nil
		}
	}
}

func listenICMP(ipv4 bool) (icmpSocket, error) {
	unprivileged, raw, address := "udp6", "ip6:ipv6-icmp", "::"
	if ipv4 {
		unprivileged, raw, address = "udp4", "ip4:icmp", "0.0.0.0"
	}

	conn, err := icmp.ListenPacket(unprivileged, address)
	if err == nil {
		return icmpSocket{conn: conn}, nil
	}

	if !errors.Is(err, os.ErrPermission) {
		return icmpSocket{}, err
	}

	conn, rawErr := icmp.ListenPacket(raw, address)
	if rawErr != nil {
		return icmpSocket{}, fmt.Errorf("no ping socket (net.ipv4.ping_group_range) and no raw socket (CAP_NET_RAW): %w", rawErr)
	}

	return icmpSocket{conn: conn, raw: true}, nil
}

func (s icmpSocket) destination(address netip.Addr) net.Addr {
	if s.raw {
		return &net.IPAddr{IP: address.AsSlice()}
	}

	return &net.UDPAddr{IP: address.AsSlice()}
}

func (s icmpSocket) isReply(data []byte, peer net.Addr, address netip.Addr, request *icmp.Echo) bool {
	from, ok := netip.AddrFromSlice(peerIP(peer))
	if !ok || from.Unmap() != address {
		return false
	}

	protocol := protocolICMPv6
	if address.Is4() {
		protocol = protocolICMP
	}

	message, err := icmp.ParseMessage(protocol, data)
	if err != nil || (message.Type != ipv4.ICMPTypeEchoReply && message.Type != ipv6.ICMPTypeEchoReply) {
		return false
	}

	echo, ok := message.Body.(*icmp.Echo)
	return ok && echo.Seq == request.Seq && (!s.raw || echo.ID == request.ID)
}

func peerIP(peer net.Addr) net.IP {
	switch address := peer.(type) {
	case *net.UDPAddr:
		return address.IP
	case *net.IPAddr:
		return address.IP
	}

	return nil
}

func (p *ICMPProbe) String() string {
	return "icmp " + p.address.String()
}

func (p *ICMPProbe) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	return oneProbeArgument(d, "icmp", &p.Address)
}

func oneProbeArgument(d *caddyfile.Dispenser, name string, target *Value) error {
	d.Next()
	var argument string
	if !d.AllArgs(&argument) {
		return d.ArgErr()
	}

	*target = Value(argument)
	if d.NextBlock(d.Nesting()) {
		return d.Errf("unknown %s check option %q", name, d.Val())
	}

	return nil
}

var (
	_ caddy.Provisioner     = (*TCPProbe)(nil)
	_ caddyfile.Unmarshaler = (*TCPProbe)(nil)
	_ Prober                = (*TCPProbe)(nil)
	_ caddy.Provisioner     = (*ICMPProbe)(nil)
	_ caddyfile.Unmarshaler = (*ICMPProbe)(nil)
	_ Prober                = (*ICMPProbe)(nil)
)
