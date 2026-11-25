package caddybgp

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/m-vinc/caddy-bgp/internal/gate"
)

var gateStates = []gate.State{
	gate.StateWaiting,
	gate.StateAnnounced,
	gate.StateWithdrawn,
	gate.StateHoldDown,
	gate.StateDrained,
	gate.StateStopping,
}

type collector struct {
	runtime *runtime

	peerUp        *prometheus.Desc
	peerFlaps     *prometheus.Desc
	peerSince     *prometheus.Desc
	peerRoutes    *prometheus.Desc
	announced     *prometheus.Desc
	state         *prometheus.Desc
	flaps         *prometheus.Desc
	transitions   *prometheus.Desc
	checkUp       *prometheus.Desc
	checkProbes   *prometheus.Desc
	checkDuration *prometheus.Desc
}

func newDesc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName("caddy", "bgp", name), help, labels, nil)
}

func newCollector(r *runtime) *collector {
	return &collector{
		runtime:       r,
		peerUp:        newDesc("peer_up", "Whether the BGP session is established.", "peer", "remote_as", "description"),
		peerFlaps:     newDesc("peer_flaps_total", "Number of times the session left established.", "peer"),
		peerSince:     newDesc("peer_established_timestamp_seconds", "Unix time the session was established.", "peer"),
		peerRoutes:    newDesc("peer_routes", "Number of routes by direction.", "peer", "direction"),
		announced:     newDesc("announced", "Whether the prefix is in the local table.", "announcement", "prefix"),
		state:         newDesc("announcement_state", "State of the announcement.", "announcement", "state"),
		flaps:         newDesc("announcement_flaps", "Flaps counted towards the hold down.", "announcement"),
		transitions:   newDesc("announcement_transitions_total", "State changes by new state.", "announcement", "state"),
		checkUp:       newDesc("check_up", "Whether the check is healthy.", "announcement", "check"),
		checkProbes:   newDesc("check_probes_total", "Probes by result.", "announcement", "check", "result"),
		checkDuration: newDesc("check_duration_seconds", "Duration of the last probe.", "announcement", "check"),
	}
}

func (c *collector) Describe(descriptions chan<- *prometheus.Desc) {
	all := []*prometheus.Desc{
		c.peerUp, c.peerFlaps, c.peerSince, c.peerRoutes, c.announced, c.state,
		c.flaps, c.transitions, c.checkUp, c.checkProbes, c.checkDuration,
	}
	for _, description := range all {
		descriptions <- description
	}
}

func gauge(description *prometheus.Desc, value float64, labels ...string) prometheus.Metric {
	return prometheus.MustNewConstMetric(description, prometheus.GaugeValue, value, labels...)
}

func counter(description *prometheus.Desc, value uint64, labels ...string) prometheus.Metric {
	return prometheus.MustNewConstMetric(description, prometheus.CounterValue, float64(value), labels...)
}

func boolean(value bool) float64 {
	if value {
		return 1
	}

	return 0
}

func (c *collector) Collect(metrics chan<- prometheus.Metric) {
	for _, peer := range c.runtime.peerStatus() {
		c.collectPeer(metrics, peer)
	}

	for _, item := range c.runtime.announcementStatus() {
		c.collectAnnouncement(metrics, item)
	}
}

func (c *collector) collectPeer(metrics chan<- prometheus.Metric, peer PeerStatus) {
	metrics <- gauge(c.peerUp, boolean(peer.Established), peer.Address, strconv.FormatUint(uint64(peer.RemoteAS), 10), peer.Description)
	metrics <- counter(c.peerFlaps, peer.Flaps, peer.Address)
	metrics <- gauge(c.peerRoutes, float64(peer.ReceivedRoutes), peer.Address, "received")
	metrics <- gauge(c.peerRoutes, float64(peer.ImportedRoutes), peer.Address, "imported")
	metrics <- gauge(c.peerRoutes, float64(len(peer.Advertised)), peer.Address, "advertised")
	if peer.Since != nil {
		metrics <- gauge(c.peerSince, float64(peer.Since.Unix()), peer.Address)
	}
}

func (c *collector) collectAnnouncement(metrics chan<- prometheus.Metric, item AnnouncementStatus) {
	metrics <- gauge(c.flaps, float64(item.Flaps), item.Name)
	for _, state := range gateStates {
		name := string(state)
		metrics <- gauge(c.state, boolean(item.State == name), item.Name, name)
		metrics <- counter(c.transitions, item.Transitions[name], item.Name, name)
	}

	for _, prefix := range item.Prefixes {
		metrics <- gauge(c.announced, boolean(prefix.Announced), item.Name, prefix.Prefix)
	}

	seen := make(map[string]bool)
	for _, check := range item.Checks {
		if seen[check.Check] {
			continue
		}

		seen[check.Check] = true
		metrics <- gauge(c.checkUp, boolean(check.Healthy), item.Name, check.Check)
		metrics <- gauge(c.checkDuration, float64(check.DurationMS)/1000, item.Name, check.Check)
		metrics <- counter(c.checkProbes, check.Successes, item.Name, check.Check, "success")
		metrics <- counter(c.checkProbes, check.Failures, item.Name, check.Check, "failure")
	}
}

var _ prometheus.Collector = (*collector)(nil)
