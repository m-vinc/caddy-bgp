package logs

import (
	"context"
	"log/slog"
	"sync/atomic"
)

type handlerBox struct {
	handler slog.Handler
}

type Target struct {
	current atomic.Pointer[handlerBox]
}

func NewTarget(handler slog.Handler) *Target {
	target := &Target{}
	target.Set(handler)
	return target
}

func (t *Target) Set(handler slog.Handler) {
	t.current.Store(&handlerBox{handler: handler})
}

func (t *Target) load() slog.Handler {
	return t.current.Load().handler
}

type derivation func(slog.Handler) slog.Handler

type Relay struct {
	target      *Target
	derivations []derivation
	demoteInfo  bool
}

func NewRelay(target *Target, demoteInfo bool) *Relay {
	return &Relay{target: target, demoteInfo: demoteInfo}
}

func (r *Relay) level(level slog.Level) slog.Level {
	if r.demoteInfo && level == slog.LevelInfo {
		return slog.LevelDebug
	}

	return level
}

func (r *Relay) Enabled(ctx context.Context, level slog.Level) bool {
	return r.target.load().Enabled(ctx, r.level(level))
}

func (r *Relay) Handle(ctx context.Context, record slog.Record) error {
	record.Level = r.level(record.Level)
	handler := r.target.load()
	for _, derive := range r.derivations {
		handler = derive(handler)
	}

	return handler.Handle(ctx, record)
}

func (r *Relay) WithAttrs(attributes []slog.Attr) slog.Handler {
	return r.derive(func(handler slog.Handler) slog.Handler { return handler.WithAttrs(attributes) })
}

func (r *Relay) WithGroup(name string) slog.Handler {
	return r.derive(func(handler slog.Handler) slog.Handler { return handler.WithGroup(name) })
}

func (r *Relay) derive(next derivation) *Relay {
	derivations := make([]derivation, 0, len(r.derivations)+1)
	derivations = append(derivations, r.derivations...)
	derivations = append(derivations, next)
	return &Relay{target: r.target, derivations: derivations, demoteInfo: r.demoteInfo}
}
