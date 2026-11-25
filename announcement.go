package caddybgp

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/m-vinc/caddy-bgp/internal/gate"
	"github.com/m-vinc/caddy-bgp/internal/speaker"
)

type probeCheck struct {
	check       *Check
	tracker     *gate.Tracker
	lastError   string
	lastChecked time.Time
	duration    time.Duration
	successes   uint64
	failures    uint64
}

type announcement struct {
	name        string
	routes      []speaker.Route
	gate        *gate.Gate
	checks      []*probeCheck
	cancel      context.CancelFunc
	ctx         context.Context
	retry       *time.Timer
	transitions map[gate.State]uint64
}

func (r *runtime) applyAnnouncements(planned *plan) error {
	wanted := make(map[string]bool)
	prefixes := make(map[netip.Prefix]bool)
	for _, item := range planned.announcements {
		wanted[item.name] = true
		for _, route := range item.routes {
			prefixes[route.Prefix] = true
		}
	}

	previous := make(map[netip.Prefix]string)
	for name, item := range r.announcements {
		for _, route := range item.routes {
			previous[route.Prefix] = name
		}

		item.halt()
		if !wanted[name] {
			delete(r.announcements, name)
			delete(r.drained, name)
		}
	}

	var failures []error

	for _, prefix := range r.speaker.Prefixes() {
		if !prefixes[prefix] {
			failures = append(failures, r.withdraw(prefix, previous[prefix], "removed from the configuration"))
		}
	}

	for _, item := range planned.announcements {
		r.configure(item)
	}

	for _, item := range planned.announcements {
		failures = append(failures, r.evaluate(r.announcements[item.name], "configuration"))
	}

	for _, item := range planned.announcements {
		r.startChecks(r.announcements[item.name])
	}

	return errors.Join(failures...)
}

func (r *runtime) configure(planned plannedAnnouncement) {
	item, exists := r.announcements[planned.name]
	if !exists {
		item = &announcement{
			name:        planned.name,
			gate:        gate.New(planned.settings, time.Now()),
			transitions: make(map[gate.State]uint64),
		}
		r.announcements[planned.name] = item
	}

	inherited := exists && item.healthy()
	previous := item.checks
	item.routes = planned.routes
	item.gate.Configure(planned.settings)
	item.checks = make([]*probeCheck, 0, len(planned.checks))
	for _, check := range planned.checks {
		item.checks = append(item.checks, adoptCheck(check, previous, inherited))
	}

	item.ctx, item.cancel = context.WithCancel(context.Background())
}

func adoptCheck(check *Check, previous []*probeCheck, inherited bool) *probeCheck {
	result := &probeCheck{check: check, tracker: gate.NewTracker(check.Rise, check.Fall, inherited)}
	for _, candidate := range previous {
		if candidate.check.key != check.key {
			continue
		}

		result.tracker.Adopt(candidate.tracker)
		result.lastError = candidate.lastError
		result.lastChecked = candidate.lastChecked
		result.duration = candidate.duration
		result.successes = candidate.successes
		result.failures = candidate.failures
		break
	}

	return result
}

func (a *announcement) healthy() bool {
	for _, check := range a.checks {
		if !check.tracker.Healthy() {
			return false
		}
	}

	return true
}

func (a *announcement) halt() {
	if a.cancel != nil {
		a.cancel()
	}

	if a.retry != nil {
		a.retry.Stop()
		a.retry = nil
	}
}

func (a *announcement) reason() string {
	var failing []string

	for _, check := range a.checks {
		if !check.tracker.Healthy() {
			failing = append(failing, check.check.name+": "+check.lastError)
		}
	}

	if len(failing) == 0 {
		return "healthy"
	}

	return strings.Join(failing, "; ")
}

func (r *runtime) startChecks(item *announcement) {
	for _, check := range item.checks {
		go r.watch(item.ctx, item, check)
	}
}

func (r *runtime) watch(ctx context.Context, item *announcement, check *probeCheck) {
	ticker := time.NewTicker(time.Duration(check.check.Interval))
	defer ticker.Stop()

	for {
		started := time.Now()
		err := check.check.run(ctx)
		r.record(ctx, item, check, err, time.Since(started))

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *runtime) record(ctx context.Context, item *announcement, check *probeCheck, err error, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ctx.Err() != nil {
		return
	}

	check.lastChecked = time.Now()
	check.duration = duration
	check.lastError = ""
	if err != nil {
		check.lastError = err.Error()
		check.failures++
	} else {
		check.successes++
	}

	if check.tracker.Record(err == nil) {
		r.logCheck(item, check)
	}

	if err := r.evaluate(item, item.reason()); err != nil {
		r.logger.Error("could not update the announcement", zap.String("announcement", item.name), zap.Error(err))
	}
}

func (r *runtime) logCheck(item *announcement, check *probeCheck) {
	fields := []zap.Field{zap.String("announcement", item.name), zap.String("check", check.check.name)}
	if check.tracker.Healthy() {
		r.logger.Info("check is healthy", fields...)
		return
	}

	r.logger.Warn("check is unhealthy", append(fields, zap.String("error", check.lastError))...)
}

func (r *runtime) evaluateAll(reason string) {
	for _, item := range r.announcements {
		if err := r.evaluate(item, reason); err != nil {
			r.logger.Error("could not update the announcement", zap.String("announcement", item.name), zap.Error(err))
		}
	}
}

func (r *runtime) evaluate(item *announcement, reason string) error {
	before := item.gate.State()
	decision := item.gate.Decide(gate.Input{
		Now:      time.Now(),
		Healthy:  item.healthy(),
		Drained:  r.drainAll || r.drained[item.name],
		Stopping: r.stopping,
	})
	if decision.State != before {
		item.transitions[decision.State]++
		r.logTransition(item, before, decision, reason)
	}

	r.scheduleRetry(item, decision.RetryIn)

	var failures []error

	for _, route := range item.routes {
		if decision.Announce {
			failures = append(failures, r.announce(item, route))
			continue
		}

		failures = append(failures, r.withdraw(route.Prefix, item.name, reason))
	}

	return errors.Join(failures...)
}

func (r *runtime) logTransition(item *announcement, before gate.State, decision gate.Decision, reason string) {
	fields := []zap.Field{
		zap.String("announcement", item.name),
		zap.String("from", string(before)),
		zap.String("to", string(decision.State)),
		zap.String("reason", reason),
	}
	if decision.RetryIn > 0 {
		fields = append(fields, zap.Duration("hold_down", decision.RetryIn), zap.Int("flaps", item.gate.Flaps()))
	}

	r.logger.Info("announcement changed state", fields...)
}

func (r *runtime) scheduleRetry(item *announcement, delay time.Duration) {
	if item.retry != nil {
		item.retry.Stop()
		item.retry = nil
	}

	if delay > 0 {
		ctx := item.ctx
		item.retry = time.AfterFunc(delay, func() { r.retry(ctx, item) })
	}
}

func (r *runtime) retry(ctx context.Context, item *announcement) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ctx.Err() != nil {
		return
	}

	if err := r.evaluate(item, "hold down expired"); err != nil {
		r.logger.Error("could not update the announcement", zap.String("announcement", item.name), zap.Error(err))
	}
}

func (r *runtime) announce(item *announcement, route speaker.Route) error {
	changed, err := r.speaker.Announce(route)
	if changed {
		nextHop := "self"
		if route.NextHop.IsValid() {
			nextHop = route.NextHop.String()
		}

		r.logger.Info("announced",
			zap.String("announcement", item.name),
			zap.String("prefix", route.Prefix.String()),
			zap.String("next_hop", nextHop))
	}

	return err
}

func (r *runtime) withdraw(prefix netip.Prefix, name, reason string) error {
	changed, err := r.speaker.Withdraw(prefix)
	if changed {
		r.logger.Warn("withdrew",
			zap.String("announcement", name),
			zap.String("prefix", prefix.String()),
			zap.String("reason", reason))
	}

	return err
}
