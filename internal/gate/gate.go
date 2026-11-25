package gate

import "time"

type State string

const (
	StateWaiting   State = "waiting"
	StateAnnounced State = "announced"
	StateWithdrawn State = "withdrawn"
	StateHoldDown  State = "hold_down"
	StateDrained   State = "drained"
	StateStopping  State = "stopping"
)

type Settings struct {
	HoldDown    time.Duration
	MaxHoldDown time.Duration
	FlapWindow  time.Duration
}

type Input struct {
	Now      time.Time
	Healthy  bool
	Drained  bool
	Stopping bool
}

type Decision struct {
	Announce bool
	State    State
	RetryIn  time.Duration
}

type Gate struct {
	settings    Settings
	state       State
	since       time.Time
	announcedAt time.Time
	heldUntil   time.Time
	flaps       int
}

func New(settings Settings, now time.Time) *Gate {
	return &Gate{settings: settings, state: StateWaiting, since: now}
}

func (g *Gate) Configure(settings Settings) {
	g.settings = settings
}

func (g *Gate) State() State {
	return g.state
}

func (g *Gate) Since() time.Time {
	return g.since
}

func (g *Gate) Flaps() int {
	return g.flaps
}

func (g *Gate) HeldUntil() time.Time {
	return g.heldUntil
}

func (g *Gate) Decide(input Input) Decision {
	next, retry := g.next(input)
	if g.state == StateAnnounced && next == StateWithdrawn {
		g.penalize(input.Now)
	}

	if next != g.state {
		g.state = next
		g.since = input.Now
		if next == StateAnnounced {
			g.announcedAt = input.Now
		}
	}

	return Decision{Announce: next == StateAnnounced, State: next, RetryIn: retry}
}

func (g *Gate) next(input Input) (State, time.Duration) {
	switch {
	case input.Stopping:
		return StateStopping, 0
	case input.Drained:
		return StateDrained, 0
	case !input.Healthy && g.state == StateWaiting:
		return StateWaiting, 0
	case !input.Healthy:
		return StateWithdrawn, 0
	case g.state != StateAnnounced && input.Now.Before(g.heldUntil):
		return StateHoldDown, g.heldUntil.Sub(input.Now)
	default:
		return StateAnnounced, 0
	}
}

func (g *Gate) penalize(now time.Time) {
	if g.settings.FlapWindow > 0 && now.Sub(g.announcedAt) >= g.settings.FlapWindow {
		g.flaps = 0
	}

	g.flaps++
	g.heldUntil = now.Add(g.penalty())
}

func (g *Gate) penalty() time.Duration {
	hold := g.settings.HoldDown
	ceiling := max(g.settings.MaxHoldDown, g.settings.HoldDown)
	for range g.flaps - 1 {
		hold *= 2
		if hold >= ceiling {
			return ceiling
		}
	}

	return hold
}
