package caddybgp

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyevents"
	"go.uber.org/zap"
)

const poolKey = "bgp"

var pool = caddy.NewUsagePool()

func init() {
	caddy.RegisterModule(App{})
}

type App struct {
	Config

	plan    *plan
	runtime *runtime
	logger  *zap.Logger
	slogger *slog.Logger
}

type stopHandler struct {
	app *App
}

func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "bgp", New: func() caddy.Module { return new(App) }}
}

func newRuntimeValue() (caddy.Destructor, error) {
	return newRuntime(), nil
}

func (a *App) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger()
	a.slogger = ctx.Slogger()
	planned, err := a.Config.plan(caddy.NewReplacer())
	if err != nil {
		return err
	}

	for _, announcement := range a.Announcements {
		for index, check := range announcement.Checks {
			if err := check.provision(ctx); err != nil {
				return fmt.Errorf("announcement %s: check %d: %w", announcement.Name, index, err)
			}
		}
	}

	a.plan = planned
	events, err := ctx.App("events")
	if err != nil {
		return fmt.Errorf("events app: %w", err)
	}

	if err := events.(*caddyevents.App).On("stopping", stopHandler{app: a}); err != nil {
		return fmt.Errorf("subscribe to the stopping event: %w", err)
	}

	shared, _, err := pool.LoadOrNew(poolKey, newRuntimeValue)
	if err != nil {
		return err
	}

	a.runtime = shared.(*runtime)
	if registry := ctx.GetMetricsRegistry(); registry != nil {
		if err := registry.Register(newCollector(a.runtime)); err != nil {
			return fmt.Errorf("register metrics: %w", err)
		}
	}

	return nil
}

func (a *App) Start() error {
	return a.runtime.activate(a)
}

func (a *App) Stop() error {
	return a.runtime.deactivate(a)
}

func (a *App) Cleanup() error {
	if a.runtime == nil {
		return nil
	}

	err := a.runtime.deactivate(a)
	if _, poolErr := pool.Delete(poolKey); poolErr != nil && err == nil {
		err = poolErr
	}

	a.runtime = nil
	return err
}

func (h stopHandler) Handle(context.Context, caddy.Event) error {
	h.app.runtime.drainForStop(h.app)
	return nil
}

func sharedRuntime() *runtime {
	var found *runtime

	pool.Range(func(_, value any) bool {
		found = value.(*runtime)
		return false
	})
	return found
}

var (
	_ caddy.App           = (*App)(nil)
	_ caddy.Provisioner   = (*App)(nil)
	_ caddy.CleanerUpper  = (*App)(nil)
	_ caddyevents.Handler = stopHandler{}
	_ caddy.Destructor    = (*runtime)(nil)
)
