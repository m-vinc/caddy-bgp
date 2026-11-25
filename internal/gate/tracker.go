package gate

type Tracker struct {
	rise    int
	fall    int
	healthy bool
	streak  int
	last    bool
}

func NewTracker(rise, fall int, healthy bool) *Tracker {
	return &Tracker{rise: max(rise, 1), fall: max(fall, 1), healthy: healthy}
}

func (t *Tracker) Record(success bool) bool {
	if success != t.last || t.streak == 0 {
		t.last = success
		t.streak = 0
	}

	t.streak++

	before := t.healthy
	if success && t.streak >= t.rise {
		t.healthy = true
	}

	if !success && t.streak >= t.fall {
		t.healthy = false
	}

	return before != t.healthy
}

func (t *Tracker) Healthy() bool {
	return t.healthy
}

func (t *Tracker) Streak() (bool, int) {
	return t.last, t.streak
}

func (t *Tracker) Adopt(previous *Tracker) {
	t.healthy = previous.healthy
	t.streak = previous.streak
	t.last = previous.last
}
