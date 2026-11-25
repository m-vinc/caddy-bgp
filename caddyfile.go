package caddybgp

import (
	"encoding/json"
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("bgp", parseGlobalOption)
}

func parseGlobalOption(d *caddyfile.Dispenser, existing any) (any, error) {
	if existing != nil {
		return nil, d.Err("the bgp option is configured twice")
	}

	app := new(App)
	if err := app.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}

	return httpcaddyfile.App{Name: "bgp", Value: caddyconfig.JSON(app, nil)}, nil
}

func (a *App) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() {
		return d.ArgErr()
	}

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		if err := a.unmarshalOption(d); err != nil {
			return err
		}
	}

	return nil
}

func (a *App) unmarshalOption(d *caddyfile.Dispenser) error {
	switch d.Val() {
	case "local_as":
		return oneValue(d, &a.LocalAS)
	case "router_id":
		return oneValue(d, &a.RouterID)
	case "port":
		return oneValue(d, &a.Port)
	case "listen":
		return manyValues(d, &a.Listen)
	case "interface":
		return oneString(d, &a.Interface)
	case "drain_delay":
		return optionalDuration(d, &a.DrainDelay)
	case "peer":
		return a.unmarshalPeer(d)
	case "announce":
		return a.unmarshalAnnouncement(d)
	}

	return d.Errf("unknown bgp option %q", d.Val())
}

func (a *App) unmarshalPeer(d *caddyfile.Dispenser) error {
	peer := new(Peer)
	if err := oneValue(d, &peer.Address); err != nil {
		return err
	}

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		if err := peer.unmarshalOption(d); err != nil {
			return err
		}
	}

	a.Peers = append(a.Peers, peer)
	return nil
}

func (p *Peer) unmarshalOption(d *caddyfile.Dispenser) error {
	switch d.Val() {
	case "remote_as":
		return oneValue(d, &p.RemoteAS)
	case "description":
		return oneString(d, &p.Description)
	case "local_address":
		return oneValue(d, &p.LocalAddress)
	case "port":
		return oneValue(d, &p.Port)
	case "password":
		return oneValue(d, &p.Password)
	case "families":
		p.Families = append(p.Families, d.RemainingArgs()...)
		return nil
	case "passive":
		return flag(d, &p.Passive)
	case "optional":
		return flag(d, &p.Optional)
	case "multihop":
		return oneInteger(d, &p.MultihopTTL)
	case "ttl_security":
		return oneInteger(d, &p.TTLSecurity)
	case "timers":
		return p.unmarshalTimers(d)
	case "connect_retry":
		return oneDuration(d, &p.ConnectRetry)
	case "max_prefixes":
		return p.unmarshalMaxPrefixes(d)
	case "bfd":
		return p.unmarshalBFD(d)
	}

	return d.Errf("unknown peer option %q", d.Val())
}

func (p *Peer) unmarshalTimers(d *caddyfile.Dispenser) error {
	var keepalive, hold string
	if !d.AllArgs(&keepalive, &hold) {
		return d.ArgErr()
	}

	if err := parseDuration(d, keepalive, &p.Keepalive); err != nil {
		return err
	}

	return parseDuration(d, hold, &p.HoldTime)
}

func (p *Peer) unmarshalMaxPrefixes(d *caddyfile.Dispenser) error {
	var number int
	if err := oneInteger(d, &number); err != nil {
		return err
	}

	p.MaxPrefixes = uint32(number)
	return nil
}

func (p *Peer) unmarshalBFD(d *caddyfile.Dispenser) error {
	p.BFD = new(BFD)
	arguments := d.RemainingArgs()
	if len(arguments) > 2 {
		return d.ArgErr()
	}

	if len(arguments) > 0 {
		if err := parseDuration(d, arguments[0], &p.BFD.Interval); err != nil {
			return err
		}
	}

	if len(arguments) > 1 {
		if err := parseInteger(d, arguments[1], &p.BFD.Multiplier); err != nil {
			return err
		}
	}

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		if d.Val() != "multihop" {
			return d.Errf("unknown bfd option %q", d.Val())
		}

		if err := flag(d, &p.BFD.Multihop); err != nil {
			return err
		}
	}

	return nil
}

func (a *App) unmarshalAnnouncement(d *caddyfile.Dispenser) error {
	announcement := new(Announcement)
	if !d.NextArg() {
		return d.ArgErr()
	}

	announcement.Name = d.Val()
	for _, prefix := range d.RemainingArgs() {
		announcement.Prefixes = append(announcement.Prefixes, Value(prefix))
	}

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		if err := announcement.unmarshalOption(d); err != nil {
			return err
		}
	}

	a.Announcements = append(a.Announcements, announcement)
	return nil
}

func (a *Announcement) unmarshalOption(d *caddyfile.Dispenser) error {
	switch d.Val() {
	case "prefix":
		return manyValues(d, &a.Prefixes)
	case "next_hop":
		return manyValues(d, &a.NextHops)
	case "community":
		return manyValues(d, &a.Communities)
	case "med":
		return a.unmarshalMED(d)
	case "prepend":
		return oneInteger(d, &a.Prepend)
	case "always":
		return flag(d, &a.Always)
	case "hold_down":
		return a.unmarshalHoldDown(d)
	case "flap_window":
		return optionalDuration(d, &a.FlapWindow)
	case "check":
		return a.unmarshalCheck(d)
	}

	return d.Errf("unknown announce option %q", d.Val())
}

func (a *Announcement) unmarshalMED(d *caddyfile.Dispenser) error {
	var argument string
	if !d.AllArgs(&argument) {
		return d.ArgErr()
	}

	number, err := strconv.ParseUint(argument, 10, 32)
	if err != nil {
		return d.Errf("med: %v", err)
	}

	med := uint32(number)
	a.MED = &med
	return nil
}

func (a *Announcement) unmarshalHoldDown(d *caddyfile.Dispenser) error {
	arguments := d.RemainingArgs()
	if len(arguments) == 0 || len(arguments) > 2 {
		return d.ArgErr()
	}

	a.HoldDown = new(caddy.Duration)
	if err := parseDuration(d, arguments[0], a.HoldDown); err != nil {
		return err
	}

	if len(arguments) == 1 {
		return nil
	}

	a.MaxHoldDown = new(caddy.Duration)
	return parseDuration(d, arguments[1], a.MaxHoldDown)
}

func (a *Announcement) unmarshalCheck(d *caddyfile.Dispenser) error {
	if !d.NextArg() {
		return d.ArgErr()
	}

	check := new(Check)
	name := d.Val()
	probe, err := check.splitOptions(d)
	if err != nil {
		return err
	}

	unmarshaler, err := caddyfile.UnmarshalModule(probe, "bgp.probes."+name)
	if err != nil {
		return err
	}

	prober, ok := unmarshaler.(Prober)
	if !ok {
		return d.Errf("module bgp.probes.%s is not a probe", name)
	}

	check.ProbeRaw = caddyconfig.JSONModuleObject(prober, "probe", name, nil)
	a.Checks = append(a.Checks, check)
	return nil
}

func (c *Check) splitOptions(d *caddyfile.Dispenser) (*caddyfile.Dispenser, error) {
	head := d.Token()
	tokens := []caddyfile.Token{head}
	for d.NextArg() {
		tokens = append(tokens, d.Token())
	}

	open := head
	open.Text = "{"
	block := []caddyfile.Token{open}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		handled, err := c.unmarshalOption(d)
		if err != nil {
			return nil, err
		}

		if !handled {
			block = append(block, d.NextSegment()...)
		}
	}

	if len(block) > 1 {
		closing := block[len(block)-1]
		closing.Text = "}"
		closing.Line++
		tokens = append(tokens, append(block, closing)...)
	}

	probe := caddyfile.NewDispenser(tokens)
	probe.Next()
	return probe, nil
}

func (c *Check) unmarshalOption(d *caddyfile.Dispenser) (bool, error) {
	switch d.Val() {
	case "interval":
		return true, oneDuration(d, &c.Interval)
	case "timeout":
		return true, oneDuration(d, &c.Timeout)
	case "rise":
		return true, oneInteger(d, &c.Rise)
	case "fall":
		return true, oneInteger(d, &c.Fall)
	}

	return false, nil
}

func oneString(d *caddyfile.Dispenser, target *string) error {
	if !d.AllArgs(target) {
		return d.ArgErr()
	}

	return nil
}

func oneValue(d *caddyfile.Dispenser, target *Value) error {
	var text string
	if !d.AllArgs(&text) {
		return d.ArgErr()
	}

	*target = Value(text)
	return nil
}

func manyValues(d *caddyfile.Dispenser, target *[]Value) error {
	arguments := d.RemainingArgs()
	if len(arguments) == 0 {
		return d.ArgErr()
	}

	for _, argument := range arguments {
		*target = append(*target, Value(argument))
	}

	return nil
}

func flag(d *caddyfile.Dispenser, target *bool) error {
	if d.NextArg() {
		return d.ArgErr()
	}

	*target = true
	return nil
}

func oneInteger(d *caddyfile.Dispenser, target *int) error {
	var argument string
	if !d.AllArgs(&argument) {
		return d.ArgErr()
	}

	return parseInteger(d, argument, target)
}

func parseInteger(d *caddyfile.Dispenser, argument string, target *int) error {
	number, err := strconv.Atoi(argument)
	if err != nil || number < 1 {
		return d.Errf("%q is not a positive number", argument)
	}

	*target = number
	return nil
}

func oneDuration(d *caddyfile.Dispenser, target *caddy.Duration) error {
	var argument string
	if !d.AllArgs(&argument) {
		return d.ArgErr()
	}

	return parseDuration(d, argument, target)
}

func optionalDuration(d *caddyfile.Dispenser, target **caddy.Duration) error {
	*target = new(caddy.Duration)
	return oneDuration(d, *target)
}

func parseDuration(d *caddyfile.Dispenser, argument string, target *caddy.Duration) error {
	duration, err := caddy.ParseDuration(argument)
	if err != nil {
		return d.Errf("%q is not a duration: %v", argument, err)
	}

	*target = caddy.Duration(duration)
	return nil
}

var (
	_ caddyfile.Unmarshaler = (*App)(nil)
	_ json.Unmarshaler      = (*Value)(nil)
)
