package caddybgp

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"

	"github.com/m-vinc/caddy-bgp/internal/gate"
	"github.com/m-vinc/caddy-bgp/internal/speaker"
)

type plannedAnnouncement struct {
	name     string
	routes   []speaker.Route
	settings gate.Settings
	checks   []*Check
}

type plan struct {
	global        speaker.Global
	peers         []speaker.Peer
	descriptions  map[string]string
	announcements []plannedAnnouncement
	interfaceName string
	addresses     []netip.Addr
	drainDelay    time.Duration
}

func (c *Config) plan(replacer *caddy.Replacer) (*plan, error) {
	result := &plan{
		descriptions:  make(map[string]string),
		interfaceName: c.Interface,
		drainDelay:    pointerOr(c.DrainDelay, defaultDrainDelay),
	}
	if result.drainDelay < 0 {
		return nil, errors.New("drain_delay cannot be negative")
	}

	if len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/ \t") {
		return nil, fmt.Errorf("interface %q is not a valid interface name", c.Interface)
	}

	global, err := c.planGlobal(replacer)
	if err != nil {
		return nil, err
	}

	result.global = global
	if err := c.planPeers(replacer, result); err != nil {
		return nil, err
	}

	if err := c.planAnnouncements(replacer, result); err != nil {
		return nil, err
	}

	return result, nil
}

func (c *Config) planGlobal(replacer *caddy.Replacer) (speaker.Global, error) {
	var global speaker.Global

	localAS, err := resolveAS(c.LocalAS, replacer)
	if err != nil {
		return global, fmt.Errorf("local_as: %w", err)
	}

	routerID, err := resolveAddress(c.RouterID, replacer)
	if err != nil {
		return global, fmt.Errorf("router_id: %w", err)
	}

	if !routerID.Is4() || routerID.IsUnspecified() {
		return global, fmt.Errorf("router_id: %s is not a usable IPv4 address", routerID)
	}

	port, err := resolvePort(c.Port, replacer, defaultPort)
	if err != nil {
		return global, fmt.Errorf("port: %w", err)
	}

	global = speaker.Global{LocalAS: localAS, RouterID: routerID}
	for _, value := range c.Listen {
		address, err := resolveAddress(value, replacer)
		if err != nil {
			return global, fmt.Errorf("listen: %w", err)
		}

		if slices.Contains(global.Listen, address) {
			return global, fmt.Errorf("listen: duplicate address %s", address)
		}

		global.Listen = append(global.Listen, address)
	}

	if len(global.Listen) > 0 {
		global.Port = port
	}

	return global, nil
}

func (c *Config) planPeers(replacer *caddy.Replacer, result *plan) error {
	seen := make(map[netip.Addr]bool)
	for index, peer := range c.Peers {
		planned, skip, err := peer.plan(replacer)
		if err != nil {
			return fmt.Errorf("peer %d (%s): %w", index, peer.Address, err)
		}

		if skip {
			continue
		}

		if seen[planned.Address] {
			return fmt.Errorf("peer %s is configured twice", planned.Address)
		}

		if planned.Passive && len(result.global.Listen) == 0 {
			return fmt.Errorf("peer %s: passive needs a listen address", planned.Address)
		}

		if planned.MultihopTTL > 0 && planned.RemoteAS == result.global.LocalAS {
			return fmt.Errorf("peer %s: multihop only applies to external peers", planned.Address)
		}

		seen[planned.Address] = true
		result.descriptions[planned.Address.String()] = peer.Description
		result.peers = append(result.peers, planned)
	}

	if len(result.peers) == 0 {
		return errors.New("no peer is configured")
	}

	return nil
}

func (p *Peer) plan(replacer *caddy.Replacer) (speaker.Peer, bool, error) {
	var planned speaker.Peer

	text, err := p.Address.resolve(replacer)
	if err != nil {
		return planned, false, err
	}

	if text == "" && p.Optional {
		return planned, true, nil
	}

	if planned.Address, err = parseAddress(text); err != nil {
		return planned, false, err
	}

	if planned.RemoteAS, err = resolveAS(p.RemoteAS, replacer); err != nil {
		return planned, false, fmt.Errorf("remote_as: %w", err)
	}

	if planned.Port, err = resolvePort(p.Port, replacer, 0); err != nil {
		return planned, false, fmt.Errorf("port: %w", err)
	}

	if planned.Password, err = p.Password.resolve(replacer); err != nil {
		return planned, false, fmt.Errorf("password: %w", err)
	}

	if p.Password != "" && planned.Password == "" {
		return planned, false, errors.New("password is configured but resolves to an empty string")
	}

	if len(planned.Password) > 80 {
		return planned, false, errors.New("password is longer than the 80 bytes TCP MD5 allows")
	}

	if p.LocalAddress != "" {
		if planned.LocalAddress, err = resolveAddress(p.LocalAddress, replacer); err != nil {
			return planned, false, fmt.Errorf("local_address: %w", err)
		}

		if planned.LocalAddress.Is4() != planned.Address.Is4() {
			return planned, false, fmt.Errorf("local_address %s is not in the family of the peer", planned.LocalAddress)
		}
	}

	if err := p.planFamilies(&planned); err != nil {
		return planned, false, err
	}

	if err := p.planTransport(&planned); err != nil {
		return planned, false, err
	}

	return planned, false, p.planTimers(&planned)
}

func (p *Peer) planFamilies(planned *speaker.Peer) error {
	if len(p.Families) == 0 {
		planned.IPv4 = planned.Address.Is4()
		planned.IPv6 = planned.Address.Is6()
	}

	for _, family := range p.Families {
		switch family {
		case "ipv4":
			planned.IPv4 = true
		case "ipv6":
			planned.IPv6 = true
		default:
			return fmt.Errorf("unknown family %q, use ipv4 or ipv6", family)
		}
	}

	return nil
}

func (p *Peer) planTransport(planned *speaker.Peer) error {
	if p.MultihopTTL != 0 && p.TTLSecurity != 0 {
		return errors.New("multihop and ttl_security are mutually exclusive")
	}

	if p.MultihopTTL < 0 || p.MultihopTTL > 255 || p.TTLSecurity < 0 || p.TTLSecurity > 254 {
		return errors.New("multihop takes a TTL from 1 to 255, ttl_security a number of hops from 1 to 254")
	}

	planned.Passive = p.Passive
	planned.MultihopTTL = uint8(p.MultihopTTL)
	planned.TTLHops = uint8(p.TTLSecurity)
	planned.MaxPrefixes = p.MaxPrefixes
	if p.BFD == nil {
		return nil
	}

	planned.BFDEnabled = true
	planned.BFD = speaker.BFD{
		Interval:   durationOr(p.BFD.Interval, defaultBFDInterval),
		Multiplier: uint32(p.BFD.Multiplier),
		Multihop:   p.BFD.Multihop,
	}
	if p.BFD.Multiplier == 0 {
		planned.BFD.Multiplier = defaultBFDFactor
	}

	if planned.BFD.Interval < 10*time.Millisecond || planned.BFD.Interval > time.Minute || p.BFD.Multiplier < 0 || p.BFD.Multiplier > 255 {
		return errors.New("bfd takes an interval from 10ms to 1m and a multiplier from 1 to 255")
	}

	return nil
}

func (p *Peer) planTimers(planned *speaker.Peer) error {
	planned.HoldTime = durationOr(p.HoldTime, defaultHoldTime)
	planned.Keepalive = durationOr(p.Keepalive, (planned.HoldTime / 3).Truncate(time.Second))
	planned.ConnectRetry = durationOr(p.ConnectRetry, defaultRetry)
	if planned.HoldTime < 3*time.Second || planned.HoldTime > 65535*time.Second || planned.HoldTime%time.Second != 0 {
		return errors.New("hold_time must be a whole number of seconds from 3s to 65535s")
	}

	if planned.Keepalive < time.Second || planned.Keepalive >= planned.HoldTime || planned.Keepalive%time.Second != 0 {
		return errors.New("keepalive_interval must be a whole number of seconds, at least 1s and shorter than hold_time")
	}

	if planned.ConnectRetry < 2*time.Second || planned.ConnectRetry%time.Second != 0 {
		return errors.New("connect_retry must be a whole number of seconds, at least 2s")
	}

	return nil
}

func (c *Config) planAnnouncements(replacer *caddy.Replacer, result *plan) error {
	names := make(map[string]bool)
	prefixes := make(map[netip.Prefix]string)
	for index, announcement := range c.Announcements {
		if announcement.Name == "" {
			return fmt.Errorf("announcement %d has no name", index)
		}

		if names[announcement.Name] {
			return fmt.Errorf("announcement %s is configured twice", announcement.Name)
		}

		names[announcement.Name] = true
		planned, err := announcement.plan(replacer, result)
		if err != nil {
			return fmt.Errorf("announcement %s: %w", announcement.Name, err)
		}

		for _, route := range planned.routes {
			if other, exists := prefixes[route.Prefix]; exists {
				return fmt.Errorf("prefix %s is in announcements %s and %s", route.Prefix, other, announcement.Name)
			}

			prefixes[route.Prefix] = announcement.Name
			if route.Prefix.IsSingleIP() {
				result.addresses = append(result.addresses, route.Prefix.Addr())
			}
		}

		result.announcements = append(result.announcements, planned)
	}

	return nil
}

func (a *Announcement) plan(replacer *caddy.Replacer, result *plan) (plannedAnnouncement, error) {
	planned := plannedAnnouncement{
		name:   a.Name,
		checks: a.Checks,
		settings: gate.Settings{
			HoldDown:    pointerOr(a.HoldDown, defaultHoldDown),
			MaxHoldDown: pointerOr(a.MaxHoldDown, defaultMaxHoldDown),
			FlapWindow:  pointerOr(a.FlapWindow, defaultFlapWindow),
		},
	}
	if planned.settings.HoldDown < 0 || planned.settings.MaxHoldDown < 0 || planned.settings.FlapWindow < 0 {
		return planned, errors.New("hold_down, max_hold_down and flap_window cannot be negative")
	}

	if len(a.Checks) == 0 && !a.Always {
		return planned, errors.New("no check is configured, add one or set always")
	}

	if len(a.Checks) > 0 && a.Always {
		return planned, errors.New("always cannot be combined with checks")
	}

	if a.Prepend < 0 || a.Prepend > 16 {
		return planned, errors.New("as_path_prepend must be between 0 and 16")
	}

	template, err := a.template(replacer, result)
	if err != nil {
		return planned, err
	}

	nextHops, err := a.resolveNextHops(replacer)
	if err != nil {
		return planned, err
	}

	for _, value := range a.Prefixes {
		prefix, err := resolvePrefix(value, replacer)
		if err != nil {
			return planned, err
		}

		route := template
		route.Prefix = prefix
		route.NextHop = nextHops[prefix.Addr().Is4()]
		if err := result.checkNextHop(route); err != nil {
			return planned, err
		}

		planned.routes = append(planned.routes, route)
	}

	if len(planned.routes) == 0 {
		return planned, errors.New("no prefix is configured")
	}

	return planned, nil
}

func (a *Announcement) template(replacer *caddy.Replacer, result *plan) (speaker.Route, error) {
	route := speaker.Route{Prepend: uint8(a.Prepend)}
	if a.MED != nil {
		route.MED = *a.MED
		route.HasMED = true
	}

	for _, peer := range result.peers {
		if a.Prepend > 0 && peer.RemoteAS == result.global.LocalAS {
			return route, fmt.Errorf("as_path_prepend would make the internal peer %s reject the route", peer.Address)
		}
	}

	for _, value := range a.Communities {
		text, err := value.resolve(replacer)
		if err != nil {
			return route, fmt.Errorf("community: %w", err)
		}

		if strings.Count(text, ":") == 2 {
			if _, err := bgp.ParseLargeCommunity(text); err != nil {
				return route, fmt.Errorf("community %q: %w", text, err)
			}

			route.LargeCommunities = append(route.LargeCommunities, text)
			continue
		}

		community, err := parseCommunity(text)
		if err != nil {
			return route, err
		}

		route.Communities = append(route.Communities, community)
	}

	return route, nil
}

func (a *Announcement) resolveNextHops(replacer *caddy.Replacer) (map[bool]netip.Addr, error) {
	nextHops := make(map[bool]netip.Addr)
	for _, value := range a.NextHops {
		address, err := resolveAddress(value, replacer)
		if err != nil {
			return nil, fmt.Errorf("next_hop: %w", err)
		}

		if address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() {
			return nil, fmt.Errorf("next_hop: %s is not usable", address)
		}

		if _, exists := nextHops[address.Is4()]; exists {
			return nil, errors.New("next_hop: at most one address per family")
		}

		nextHops[address.Is4()] = address
	}

	return nextHops, nil
}

func (p *plan) checkNextHop(route speaker.Route) error {
	carried := false
	for _, peer := range p.peers {
		ipv4 := route.Prefix.Addr().Is4()
		if (ipv4 && !peer.IPv4) || (!ipv4 && !peer.IPv6) {
			continue
		}

		carried = true
		if !route.NextHop.IsValid() && peer.Address.Is4() != ipv4 {
			return fmt.Errorf("prefix %s needs a next_hop, peer %s is in the other family", route.Prefix, peer.Address)
		}
	}

	if !carried {
		return fmt.Errorf("prefix %s: no peer carries its family", route.Prefix)
	}

	return nil
}

func parseCommunity(text string) (uint32, error) {
	if known, exists := bgp.WellKnownCommunityValueMap[text]; exists {
		return uint32(known), nil
	}

	high, low, found := strings.Cut(text, ":")
	if !found {
		return 0, fmt.Errorf("community %q is neither well known nor of the form 65000:100", text)
	}

	upper, err := strconv.ParseUint(high, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("community %q: %w", text, err)
	}

	lower, err := strconv.ParseUint(low, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("community %q: %w", text, err)
	}

	return uint32(upper<<16 | lower), nil
}

func resolvePrefix(value Value, replacer *caddy.Replacer) (netip.Prefix, error) {
	text, err := value.resolve(replacer)
	if err != nil {
		return netip.Prefix{}, err
	}

	if address, err := netip.ParseAddr(text); err == nil {
		text = netip.PrefixFrom(address, address.BitLen()).String()
	}

	prefix, err := netip.ParsePrefix(text)
	if err != nil {
		return netip.Prefix{}, err
	}

	if prefix != prefix.Masked() {
		return netip.Prefix{}, fmt.Errorf("prefix %s has host bits set, use %s", prefix, prefix.Masked())
	}

	if prefix.Bits() == 0 || prefix.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("prefix %s cannot be announced", prefix)
	}

	return prefix, nil
}

func resolveAddress(value Value, replacer *caddy.Replacer) (netip.Addr, error) {
	text, err := value.resolve(replacer)
	if err != nil {
		return netip.Addr{}, err
	}

	return parseAddress(text)
}

func parseAddress(text string) (netip.Addr, error) {
	address, err := netip.ParseAddr(text)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%q is not an IP address", text)
	}

	return address.Unmap(), nil
}

func resolveAS(value Value, replacer *caddy.Replacer) (uint32, error) {
	text, err := value.resolve(replacer)
	if err != nil {
		return 0, err
	}

	number, err := strconv.ParseUint(text, 10, 32)
	if err != nil || number == 0 || number == 23456 || number == 65535 || number == 4294967295 {
		return 0, fmt.Errorf("%q is not a usable AS number", text)
	}

	return uint32(number), nil
}

func resolvePort(value Value, replacer *caddy.Replacer, fallback uint16) (uint16, error) {
	if value == "" {
		return fallback, nil
	}

	text, err := value.resolve(replacer)
	if err != nil {
		return 0, err
	}

	number, err := strconv.ParseUint(text, 10, 16)
	if err != nil || number == 0 {
		return 0, fmt.Errorf("%q is not a port", text)
	}

	return uint16(number), nil
}
