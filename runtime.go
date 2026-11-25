package caddybgp

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/m-vinc/caddy-bgp/internal/anycast"
	"github.com/m-vinc/caddy-bgp/internal/logs"
	"github.com/m-vinc/caddy-bgp/internal/speaker"
)

type runtime struct {
	mu            sync.Mutex
	owners        []*App
	logger        *zap.Logger
	target        *logs.Target
	speaker       *speaker.Speaker
	announcements map[string]*announcement
	drainAll      bool
	drained       map[string]bool
	stopping      bool
	manager       *anycast.Manager
	retired       []*anycast.Manager
	prune         *time.Timer
	kernel        anycast.Kernel

	peerLoggerMu sync.Mutex
	peerLogger   *zap.Logger
}

var systemKernel = anycast.SystemKernel

func newRuntime() *runtime {
	r := &runtime{
		logger:        zap.NewNop(),
		peerLogger:    zap.NewNop(),
		target:        logs.NewTarget(slog.DiscardHandler),
		announcements: make(map[string]*announcement),
		drained:       make(map[string]bool),
		kernel:        systemKernel(),
	}
	r.speaker = speaker.New(slog.New(logs.NewRelay(r.target, true)), r.logPeerEvent)
	return r
}

func (r *runtime) Destruct() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.shutdown()
}

func (r *runtime) owner() *App {
	if len(r.owners) == 0 {
		return nil
	}

	return r.owners[len(r.owners)-1]
}

func (r *runtime) activate(app *App) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	previous := r.owner()
	if err := r.apply(app); err != nil {
		return r.restore(previous, err)
	}

	r.owners = append(r.owners, app)
	return nil
}

func (r *runtime) restore(previous *App, cause error) error {
	if previous == nil {
		return errors.Join(cause, r.shutdown())
	}

	r.logger.Error("could not apply the configuration, restoring the previous one", zap.Error(cause))
	if err := r.apply(previous); err != nil {
		return errors.Join(cause, fmt.Errorf("restore the previous configuration: %w", err))
	}

	return cause
}

func (r *runtime) deactivate(app *App) error {
	r.drainForStop(app)

	r.mu.Lock()
	defer r.mu.Unlock()

	index := slices.Index(r.owners, app)
	if index < 0 {
		return nil
	}

	wasOwner := index == len(r.owners)-1
	r.owners = slices.Delete(r.owners, index, index+1)
	if !wasOwner {
		return nil
	}

	if previous := r.owner(); previous != nil {
		r.logger.Warn("the new configuration did not start, restoring the previous one")
		return r.apply(previous)
	}

	return r.shutdown()
}

func (r *runtime) drainForStop(app *App) {
	r.mu.Lock()
	if len(r.owners) != 1 || r.owners[0] != app || r.stopping {
		r.mu.Unlock()
		return
	}

	r.stopping = true
	visible := len(r.speaker.Prefixes()) > 0 && r.speaker.EstablishedPeers() > 0
	r.evaluateAll("stopping")
	delay := app.plan.drainDelay
	logger := r.logger
	r.mu.Unlock()

	if !visible || delay == 0 {
		return
	}

	logger.Info("withdrew every prefix, draining before the listeners close", zap.Duration("drain_delay", delay))
	time.Sleep(delay)
}

func (r *runtime) shutdown() error {
	for _, item := range r.announcements {
		item.halt()
	}

	r.announcements = make(map[string]*announcement)
	r.speaker.Stop()
	if r.prune != nil {
		r.prune.Stop()
	}

	r.retireManager()
	return r.closeRetired()
}

func (r *runtime) setLoggers(app *App) {
	r.logger = app.logger
	r.target.Set(app.slogger.Handler())
	r.peerLoggerMu.Lock()
	r.peerLogger = app.logger
	r.peerLoggerMu.Unlock()
}

func (r *runtime) apply(app *App) error {
	r.setLoggers(app)
	r.stopping = false
	if err := r.applyAddresses(app.plan); err != nil {
		return err
	}

	restarted, err := r.speaker.Apply(app.plan.global, app.plan.peers)
	if restarted {
		r.logger.Warn("local_as, router_id, listen or port changed, every session was reset")
	}

	if err != nil {
		return err
	}

	if err := r.applyAnnouncements(app.plan); err != nil {
		return err
	}

	r.schedulePrune(app.plan)
	return nil
}

func (r *runtime) logPeerEvent(event speaker.PeerEvent) {
	r.peerLoggerMu.Lock()
	logger := r.peerLogger
	r.peerLoggerMu.Unlock()

	fields := []zap.Field{zap.String("peer", event.Address.String()), zap.String("state", event.State)}
	if event.Established {
		logger.Info("peer established", fields...)
		return
	}

	logger.Warn("peer lost", append(fields, zap.String("reason", event.Reason), zap.String("detail", event.Detail))...)
}
