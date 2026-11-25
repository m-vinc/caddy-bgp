package caddybgp

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/m-vinc/caddy-bgp/internal/gate"
	"github.com/m-vinc/caddy-bgp/internal/speaker"
)

type PeerStatus struct {
	speaker.PeerStatus

	Description string `json:"description,omitempty"`
}

type CheckStatus struct {
	Check       string     `json:"check"`
	Healthy     bool       `json:"healthy"`
	LastSuccess bool       `json:"last_success"`
	Streak      int        `json:"streak"`
	LastError   string     `json:"last_error,omitempty"`
	LastChecked *time.Time `json:"last_checked,omitempty"`
	DurationMS  int64      `json:"duration_ms"`
	Successes   uint64     `json:"successes"`
	Failures    uint64     `json:"failures"`
}

type PrefixStatus struct {
	Prefix    string `json:"prefix"`
	NextHop   string `json:"next_hop"`
	Announced bool   `json:"announced"`
}

type AnnouncementStatus struct {
	Name        string            `json:"name"`
	State       string            `json:"state"`
	Since       time.Time         `json:"since"`
	Healthy     bool              `json:"healthy"`
	Drained     bool              `json:"drained"`
	Flaps       int               `json:"flaps"`
	HeldUntil   *time.Time        `json:"held_until,omitempty"`
	Prefixes    []PrefixStatus    `json:"prefixes"`
	Checks      []CheckStatus     `json:"checks"`
	Transitions map[string]uint64 `json:"transitions"`
}

var (
	errUnknownAnnouncement = errors.New("unknown announcement")
	errAllDrained          = errors.New("every announcement is drained, undrain without a name")
	errNotRunning          = errors.New("bgp is not running")
)

func (r *runtime) peerStatus() []PeerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()

	owner := r.owner()
	statuses := r.speaker.Status()
	result := make([]PeerStatus, 0, len(statuses))
	for _, status := range statuses {
		entry := PeerStatus{PeerStatus: status}
		if owner != nil {
			entry.Description = owner.plan.descriptions[status.Address]
		}

		result = append(result, entry)
	}

	return result
}

func (r *runtime) announcementStatus() []AnnouncementStatus {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]AnnouncementStatus, 0, len(r.announcements))
	for _, item := range r.announcements {
		result = append(result, r.describe(item))
	}

	slices.SortFunc(result, func(a, b AnnouncementStatus) int { return strings.Compare(a.Name, b.Name) })
	return result
}

func (r *runtime) describe(item *announcement) AnnouncementStatus {
	status := AnnouncementStatus{
		Name:        item.name,
		State:       string(item.gate.State()),
		Since:       item.gate.Since(),
		Healthy:     item.healthy(),
		Drained:     r.drainAll || r.drained[item.name],
		Flaps:       item.gate.Flaps(),
		Prefixes:    make([]PrefixStatus, 0, len(item.routes)),
		Checks:      make([]CheckStatus, 0, len(item.checks)),
		Transitions: make(map[string]uint64),
	}
	if item.gate.State() == gate.StateHoldDown {
		until := item.gate.HeldUntil()
		status.HeldUntil = &until
	}

	for state, count := range item.transitions {
		status.Transitions[string(state)] = count
	}

	for _, route := range item.routes {
		entry := PrefixStatus{Prefix: route.Prefix.String(), NextHop: "self", Announced: r.speaker.Announced(route.Prefix)}
		if route.NextHop.IsValid() {
			entry.NextHop = route.NextHop.String()
		}

		status.Prefixes = append(status.Prefixes, entry)
	}

	for _, check := range item.checks {
		status.Checks = append(status.Checks, check.describe())
	}

	return status
}

func (c *probeCheck) describe() CheckStatus {
	last, streak := c.tracker.Streak()
	status := CheckStatus{
		Check:       c.check.name,
		Healthy:     c.tracker.Healthy(),
		LastSuccess: last,
		Streak:      streak,
		LastError:   c.lastError,
		DurationMS:  c.duration.Milliseconds(),
		Successes:   c.successes,
		Failures:    c.failures,
	}
	if !c.lastChecked.IsZero() {
		checked := c.lastChecked
		status.LastChecked = &checked
	}

	return status
}

func (r *runtime) setDrain(name string, drain bool) (time.Duration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	owner := r.owner()
	if owner == nil {
		return 0, errNotRunning
	}

	if err := r.markDrain(name, drain); err != nil {
		return 0, err
	}

	r.logger.Info("drain changed", zap.String("announcement", name), zap.Bool("drain", drain))
	reason := "undrained"
	if drain {
		reason = "drained"
	}

	r.evaluateAll(reason)
	return owner.plan.drainDelay, nil
}

func (r *runtime) markDrain(name string, drain bool) error {
	if name == "" {
		r.drainAll = drain
		r.drained = make(map[string]bool)
		return nil
	}

	if _, exists := r.announcements[name]; !exists {
		return fmt.Errorf("%w: %s", errUnknownAnnouncement, name)
	}

	if !drain && r.drainAll {
		return errAllDrained
	}

	if drain {
		r.drained[name] = true
		return nil
	}

	delete(r.drained, name)
	return nil
}
