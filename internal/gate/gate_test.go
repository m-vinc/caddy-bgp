package gate

import (
	"testing"
	"time"
)

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type probeStep struct {
	success bool
	healthy bool
	changed bool
}

func TestTrackerRiseAndFall(t *testing.T) {
	tracker := NewTracker(2, 3, false)
	steps := []probeStep{
		{true, false, false},
		{false, false, false},
		{true, false, false},
		{true, true, true},
		{false, true, false},
		{false, true, false},
		{true, true, false},
		{false, true, false},
		{false, true, false},
		{false, false, true},
		{false, false, false},
	}
	for index, step := range steps {
		changed := tracker.Record(step.success)
		if changed != step.changed || tracker.Healthy() != step.healthy {
			t.Fatalf("step %d: changed=%v healthy=%v, want %+v", index, changed, tracker.Healthy(), step)
		}
	}
}

func TestTrackerStartsHealthyNeedsFallFailures(t *testing.T) {
	tracker := NewTracker(2, 2, true)
	tracker.Record(false)
	if !tracker.Healthy() {
		t.Fatal("one failure must not flip a tracker that inherited a healthy state")
	}

	tracker.Record(false)
	if tracker.Healthy() {
		t.Fatal("two failures must flip the tracker")
	}
}

func TestTrackerAdoptKeepsStreak(t *testing.T) {
	previous := NewTracker(3, 2, false)
	previous.Record(true)
	previous.Record(true)
	next := NewTracker(3, 2, false)
	next.Adopt(previous)
	if !next.Record(true) || !next.Healthy() {
		t.Fatal("the third success must count the two adopted ones")
	}
}

func decide(g *Gate, at time.Duration, healthy bool) Decision {
	return g.Decide(Input{Now: start.Add(at), Healthy: healthy})
}

func TestGateWaitsUntilHealthy(t *testing.T) {
	g := New(Settings{HoldDown: 10 * time.Second}, start)
	if got := decide(g, 0, false); got.Announce || got.State != StateWaiting {
		t.Fatalf("got %+v", got)
	}

	if got := decide(g, time.Second, true); !got.Announce || got.State != StateAnnounced {
		t.Fatalf("the first announcement must not be held down: %+v", got)
	}
}

func TestGateHoldsDownAfterFlap(t *testing.T) {
	g := New(Settings{HoldDown: 10 * time.Second, MaxHoldDown: time.Minute, FlapWindow: time.Hour}, start)
	decide(g, 0, true)
	if got := decide(g, time.Second, false); got.Announce || got.State != StateWithdrawn {
		t.Fatalf("got %+v", got)
	}

	got := decide(g, 3*time.Second, true)
	if got.Announce || got.State != StateHoldDown || got.RetryIn != 8*time.Second {
		t.Fatalf("got %+v", got)
	}

	if got := decide(g, 5*time.Second, false); got.State != StateWithdrawn {
		t.Fatalf("got %+v", got)
	}

	if g.Flaps() != 1 {
		t.Fatalf("failing while held down must not count as a flap, got %d", g.Flaps())
	}

	if got := decide(g, 11*time.Second, true); !got.Announce {
		t.Fatalf("got %+v", got)
	}
}

func TestGateDoublesAndCapsHoldDown(t *testing.T) {
	g := New(Settings{HoldDown: 10 * time.Second, MaxHoldDown: 35 * time.Second, FlapWindow: time.Hour}, start)
	now := time.Duration(0)
	for index, want := range []time.Duration{10 * time.Second, 20 * time.Second, 35 * time.Second, 35 * time.Second} {
		if got := decide(g, now, true); !got.Announce {
			t.Fatalf("flap %d: not announced: %+v", index, got)
		}

		decide(g, now, false)
		if got := decide(g, now, true); got.RetryIn != want {
			t.Fatalf("flap %d: hold down %s, want %s", index, got.RetryIn, want)
		}

		now += want
	}
}

func TestGateForgetsFlapsAfterStableWindow(t *testing.T) {
	g := New(Settings{HoldDown: time.Second, MaxHoldDown: time.Minute, FlapWindow: time.Minute}, start)
	decide(g, 0, true)
	decide(g, time.Second, false)
	decide(g, 2*time.Second, true)
	decide(g, 3*time.Second, false)
	if g.Flaps() != 2 {
		t.Fatalf("got %d flaps", g.Flaps())
	}

	decide(g, 10*time.Second, true)
	decide(g, 2*time.Minute, false)
	if g.Flaps() != 1 {
		t.Fatalf("got %d flaps after a stable minute", g.Flaps())
	}
}

func TestGateZeroHoldDownReannouncesAtOnce(t *testing.T) {
	g := New(Settings{}, start)
	decide(g, 0, true)
	decide(g, time.Second, false)
	if got := decide(g, 2*time.Second, true); !got.Announce {
		t.Fatalf("got %+v", got)
	}
}

func TestGateDrainIsNotAFlap(t *testing.T) {
	g := New(Settings{HoldDown: time.Minute}, start)
	decide(g, 0, true)
	if got := g.Decide(Input{Now: start, Healthy: true, Drained: true}); got.Announce || got.State != StateDrained {
		t.Fatalf("got %+v", got)
	}

	if got := decide(g, time.Second, true); !got.Announce || g.Flaps() != 0 {
		t.Fatalf("undrain must announce at once: %+v flaps=%d", got, g.Flaps())
	}
}

func TestGateStoppingWins(t *testing.T) {
	g := New(Settings{}, start)
	decide(g, 0, true)
	got := g.Decide(Input{Now: start, Healthy: true, Drained: true, Stopping: true})
	if got.Announce || got.State != StateStopping {
		t.Fatalf("got %+v", got)
	}
}

func TestGateUnhealthyWhileDrainedStaysDownAfterUndrain(t *testing.T) {
	g := New(Settings{HoldDown: time.Minute}, start)
	decide(g, 0, true)
	g.Decide(Input{Now: start, Healthy: false, Drained: true})
	if got := decide(g, time.Second, false); got.Announce || got.State != StateWithdrawn {
		t.Fatalf("got %+v", got)
	}
}
