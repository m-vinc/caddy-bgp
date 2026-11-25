package caddybgp

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
)

const (
	defaultPort        = 179
	defaultHoldTime    = 9 * time.Second
	defaultRetry       = 5 * time.Second
	defaultDrainDelay  = 5 * time.Second
	defaultHoldDown    = 10 * time.Second
	defaultMaxHoldDown = time.Minute
	defaultFlapWindow  = 5 * time.Minute
	defaultInterval    = 5 * time.Second
	defaultTimeout     = 2 * time.Second
	defaultRise        = 2
	defaultFall        = 2
	defaultBFDInterval = 300 * time.Millisecond
	defaultBFDFactor   = 3
)

type Value string

func (v *Value) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*v = Value(text)
		return nil
	}

	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return fmt.Errorf("expected a string or a number: %s", data)
	}

	*v = Value(number.String())
	return nil
}

func (v Value) resolve(replacer *caddy.Replacer) (string, error) {
	text, err := replacer.ReplaceOrErr(string(v), false, true)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(text), nil
}

type BFD struct {
	Interval   caddy.Duration `json:"interval,omitempty"`
	Multiplier int            `json:"multiplier,omitempty"`
	Multihop   bool           `json:"multihop,omitempty"`
}

type Peer struct {
	Address      Value          `json:"address"`
	RemoteAS     Value          `json:"remote_as"`
	Description  string         `json:"description,omitempty"`
	LocalAddress Value          `json:"local_address,omitempty"`
	Port         Value          `json:"port,omitempty"`
	Families     []string       `json:"families,omitempty"`
	Password     Value          `json:"password,omitempty"`
	Passive      bool           `json:"passive,omitempty"`
	MultihopTTL  int            `json:"multihop_ttl,omitempty"`
	TTLSecurity  int            `json:"ttl_security_hops,omitempty"`
	HoldTime     caddy.Duration `json:"hold_time,omitempty"`
	Keepalive    caddy.Duration `json:"keepalive_interval,omitempty"`
	ConnectRetry caddy.Duration `json:"connect_retry,omitempty"`
	MaxPrefixes  uint32         `json:"max_prefixes,omitempty"`
	BFD          *BFD           `json:"bfd,omitempty"`
	Optional     bool           `json:"optional,omitempty"`
}

type Check struct {
	Interval caddy.Duration  `json:"interval,omitempty"`
	Timeout  caddy.Duration  `json:"timeout,omitempty"`
	Rise     int             `json:"rise,omitempty"`
	Fall     int             `json:"fall,omitempty"`
	ProbeRaw json.RawMessage `json:"probe,omitempty" caddy:"namespace=bgp.probes inline_key=probe"`

	probe Prober
	name  string
	key   string
}

type Announcement struct {
	Name        string          `json:"name"`
	Prefixes    []Value         `json:"prefixes"`
	NextHops    []Value         `json:"next_hops,omitempty"`
	Communities []Value         `json:"communities,omitempty"`
	MED         *uint32         `json:"med,omitempty"`
	Prepend     int             `json:"as_path_prepend,omitempty"`
	Checks      []*Check        `json:"checks,omitempty"`
	Always      bool            `json:"always,omitempty"`
	HoldDown    *caddy.Duration `json:"hold_down,omitempty"`
	MaxHoldDown *caddy.Duration `json:"max_hold_down,omitempty"`
	FlapWindow  *caddy.Duration `json:"flap_window,omitempty"`
}

type Config struct {
	LocalAS       Value           `json:"local_as"`
	RouterID      Value           `json:"router_id"`
	Listen        []Value         `json:"listen,omitempty"`
	Port          Value           `json:"port,omitempty"`
	Interface     string          `json:"interface,omitempty"`
	DrainDelay    *caddy.Duration `json:"drain_delay,omitempty"`
	Peers         []*Peer         `json:"peers,omitempty"`
	Announcements []*Announcement `json:"announcements,omitempty"`
}

func durationOr(value caddy.Duration, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}

	return time.Duration(value)
}

func pointerOr(value *caddy.Duration, fallback time.Duration) time.Duration {
	if value == nil {
		return fallback
	}

	return time.Duration(*value)
}
