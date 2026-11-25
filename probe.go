package caddybgp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/caddyserver/caddy/v2"
)

type Prober interface {
	Probe(ctx context.Context) error
}

func (c *Check) provision(ctx caddy.Context) error {
	c.key = string(c.ProbeRaw)
	if len(c.ProbeRaw) == 0 {
		return errors.New("no probe is configured")
	}

	loaded, err := ctx.LoadModule(c, "ProbeRaw")
	if err != nil {
		return fmt.Errorf("load probe: %w", err)
	}

	prober, ok := loaded.(Prober)
	if !ok {
		return fmt.Errorf("%T is not a probe", loaded)
	}

	c.probe = prober
	c.name = probeName(prober)
	if c.Interval == 0 {
		c.Interval = caddy.Duration(defaultInterval)
	}

	if c.Timeout == 0 {
		c.Timeout = caddy.Duration(defaultTimeout)
	}

	if c.Rise == 0 {
		c.Rise = defaultRise
	}

	if c.Fall == 0 {
		c.Fall = defaultFall
	}

	return c.validate()
}

func (c *Check) validate() error {
	if time.Duration(c.Interval) < 100*time.Millisecond {
		return errors.New("interval must be at least 100ms")
	}

	if c.Timeout < 0 || c.Rise < 1 || c.Fall < 1 {
		return errors.New("timeout, rise and fall must be positive")
	}

	return nil
}

func probeName(prober Prober) string {
	if named, ok := prober.(fmt.Stringer); ok {
		return named.String()
	}

	if module, ok := prober.(caddy.Module); ok {
		return module.CaddyModule().ID.Name()
	}

	return fmt.Sprintf("%T", prober)
}

func (c *Check) run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Timeout))
	defer cancel()

	return c.probe.Probe(ctx)
}
