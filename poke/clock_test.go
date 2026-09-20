package poke

import (
	"testing"
	"time"
)

// TestClockCorrectsTheHost is the reason this service hits a second at all. Every guard in the
// treasury compares against now(), the gen_utime of the block carrying the external. A host clock
// a few seconds fast fires early on every deadline and loses a full retry interval to it, every
// round, with nothing in the logs to say so.
func TestClockCorrectsTheHost(t *testing.T) {
	for _, skew := range []int64{-90, -1, 0, 1, 90} {
		var c Clock
		if c.Synced() {
			t.Fatal("a fresh clock claimed to be synced")
		}
		chainNow := uint32(time.Now().Unix() + skew)
		c.Observe(chainNow)

		if !c.Synced() {
			t.Fatal("observing the chain did not mark the clock synced")
		}
		if got := c.Offset(); got != skew {
			t.Fatalf("offset %v, want %v", got, skew)
		}
		if got := int64(c.Now()) - int64(chainNow); got < -1 || got > 1 {
			t.Fatalf("corrected now is %v seconds from the chain's", got)
		}

		// A deadline one minute out is one minute out on the chain's clock, not the host's.
		until := c.Until(chainNow + 60)
		if until < 59*time.Second || until > 61*time.Second {
			t.Fatalf("with %v seconds of skew, Until said %v", skew, until)
		}
	}
}

// TestNextWait pins the cadence: the burst around a deadline, and the plain minute outside it.
func TestNextWait(t *testing.T) {
	tests := []struct {
		name           string
		pending        bool
		sent           bool
		sinceFirstSend time.Duration
		untilDeadline  time.Duration
		haveDeadline   bool
		want           time.Duration
	}{
		{
			name: "just sent, stay on the burst cadence",
			// The poke went out at the deadline; keep trying every second in case the block that
			// carried it was produced a moment early.
			pending: true, sent: true, sinceFirstSend: time.Second, want: BurstTick,
		},
		{
			name:    "the burst has closed, fall back to the minute",
			pending: true, sent: true, sinceFirstSend: BurstTail + time.Second, want: RetryInterval,
		},
		{
			name:          "a deadline inside the lead opens the burst",
			untilDeadline: BurstLead - time.Millisecond, haveDeadline: true, want: BurstTick,
		},
		{
			name:          "a deadline already passed but nothing due yet",
			untilDeadline: -5 * time.Second, haveDeadline: true, want: BurstTick,
		},
		{
			name:          "sleep until the burst would open",
			untilDeadline: 10 * time.Second, haveDeadline: true, want: 10*time.Second - BurstLead,
		},
		{
			name:          "a distant deadline is capped so a config change is still noticed",
			untilDeadline: 6 * time.Hour, haveDeadline: true, want: MaxSleep,
		},
		{
			name: "nothing due and no deadline known",
			want: RetryInterval,
		},
		{
			name: "an outstanding poke outranks an approaching deadline",
			// Otherwise the loop would sleep towards the next transition while the current one is
			// still unacknowledged.
			pending: true, sent: true, sinceFirstSend: 10 * time.Minute,
			untilDeadline: 2 * time.Hour, haveDeadline: true, want: RetryInterval,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := NextWait(tt.pending, tt.sent, tt.sinceFirstSend, tt.untilDeadline, tt.haveDeadline)
			if got != tt.want {
				t.Fatalf("got %v (%v), want %v", got, reason, tt.want)
			}
		})
	}
}

// The burst is only worth its noise if it actually brackets the deadline. Walking a clock through
// it second by second is what proves the loop is awake on both sides.
//
// NextDeadline only ever reports a deadline still in the future, so once the moment passes the
// loop is driven by the now-outstanding poke rather than by a deadline. The fixture models that.
func TestBurstBracketsTheDeadline(t *testing.T) {
	var c Clock
	base := time.Now().Unix()
	c.Observe(uint32(base))
	deadline := uint32(base + 30)

	awakeAt := map[int64]bool{}
	for offset := int64(-5); offset <= 5; offset++ {
		c.Observe(uint32(base + 30 + offset))
		pending := offset >= 0 // the poke becomes due at the deadline and is sent from then on
		var since time.Duration
		if pending {
			since = time.Duration(offset) * time.Second
		}
		wait, _ := NextWait(pending, pending, since, c.Until(deadline), !pending)
		awakeAt[offset] = wait == BurstTick
	}

	for offset := int64(-2); offset <= 2; offset++ {
		if !awakeAt[offset] {
			t.Fatalf("the loop was not on the burst cadence at deadline%+d", offset)
		}
	}
	if awakeAt[5] {
		t.Fatal("the burst never closed")
	}
}

// TestNextWaitDoesNotSleepPastASecondTransition is the regression for the bug that disabled the
// headline feature. A round wedged for ten minutes must not stop the loop from waking for another
// round whose deadline is seconds away.
func TestNextWaitDoesNotSleepPastASecondTransition(t *testing.T) {
	// A poke outstanding for ten minutes - the state PokerPokeUnconfirmed fires on - and another
	// round's transition twenty seconds out.
	wait, reason := NextWait(true, true, 10*time.Minute, 20*time.Second, true)
	if wait != 20*time.Second-BurstLead {
		t.Fatalf("with a wedged round pending, a deadline %v away produced a %v sleep (%v); "+
			"the second transition would wait for the retry interval", 20*time.Second, wait, reason)
	}

	// The same, with the deadline already inside the lead.
	if wait, _ := NextWait(true, true, 10*time.Minute, time.Second, true); wait != BurstTick {
		t.Fatalf("an imminent deadline did not open the burst while a poke was outstanding: %v", wait)
	}

	// And the burst tail still wins outright: nothing is sooner than one second.
	if wait, _ := NextWait(true, true, time.Second, 20*time.Second, true); wait != BurstTick {
		t.Fatalf("a just-sent poke left the burst cadence: %v", wait)
	}

	// A far deadline must not drag a pending retry out to MaxSleep.
	if wait, r := NextWait(true, true, 10*time.Minute, 4*time.Hour, true); wait != RetryInterval {
		t.Fatalf("a pending poke waited %v (%v), want the retry interval", wait, r)
	}
}

// TestScheduleUsesTheNewestSend covers the bug at its real site.
//
// NextWait had the right rule and `schedule` handed it the wrong number: the oldest outstanding
// age rather than the newest. A unit test on NextWait alone passed throughout, which is why this
// one exists at the wiring instead.
func TestScheduleUsesTheNewestSend(t *testing.T) {
	p := &Poker{clock: &Clock{}, tracker: NewTracker()}
	p.clock.Observe(testNow)
	wall := time.Now()

	// A round wedged for ten minutes, exactly the state PokerPokeUnconfirmed fires on.
	wedged := Poke{Op: OpParticipateInElection, RoundSince: nextRound}
	p.tracker.Observe([]Poke{wedged}, []Poke{wedged}, wall.Add(-10*time.Minute))

	// Meanwhile another round's stake_held_until is twenty seconds away.
	v := trustedView(t, prevRound, StateHeld, currentHash, testNow+20, false, electionAt, testNow)

	wait, reason := p.schedule(v, wall, true)
	if wait > 20*time.Second {
		t.Fatalf("slept %v (%v) past a transition %v away, because another round is wedged",
			wait, reason, 20*time.Second)
	}

	// And the burst tail: a second poke sent THIS cycle, alongside the wedged one. The newest age
	// is zero and the cadence must be one second; the oldest age is ten minutes and would put the
	// loop on the plain retry, losing the burst for the round that just became due.
	fresh := Poke{Op: OpFinishParticipation, RoundSince: prevRound}
	p.tracker.Observe([]Poke{wedged, fresh}, []Poke{fresh}, wall)
	if wait, reason := p.schedule(v, wall, true); wait != BurstTick {
		t.Fatalf("a poke sent this cycle did not hold the burst cadence: %v (%v); "+
			"the oldest outstanding poke is being used instead of the newest", wait, reason)
	}
}

// TestNextWaitDoesNotSpinOnAnEmptyTracker. An empty tracker reports an age of zero, and zero is
// indistinguishable from "sent this instant" unless the caller says whether anything was sent at
// all. Without that the loop pins itself at one second forever in the two states where a hot loop
// is least affordable: every send failing, and DRY_RUN - neither of which records a send, so
// neither of which ever ages out of the burst.
func TestNextWaitDoesNotSpinOnAnEmptyTracker(t *testing.T) {
	if wait, reason := NextWait(true, false, 0, 0, false); wait != RetryInterval {
		t.Fatalf("with nothing sent the loop waits %v (%v); it would re-read the chain every second",
			wait, reason)
	}
	if wait, _ := NextWait(true, false, 0, 4*time.Hour, true); wait != RetryInterval {
		t.Fatalf("with nothing sent and a distant deadline the loop waited %v", wait)
	}
	// And the burst still works for a poke that really was sent this instant.
	if wait, _ := NextWait(true, true, 0, 0, false); wait != BurstTick {
		t.Fatalf("a genuinely just-sent poke lost the burst: %v", wait)
	}
}

// TestWaitWhileUnreadableDoesNotSleepThroughARotation. A cycle that could not read the treasury
// knows nothing about any round - except when the next validator set rotates, which comes from
// config 34 and is still readable. That is when vset_changed becomes legal for every validating
// round, and sleeping a flat minute through it gives back exactly the lateness the burst exists
// to remove.
func TestWaitWhileUnreadableDoesNotSleepThroughARotation(t *testing.T) {
	tests := []struct {
		name          string
		untilRotation time.Duration
		want          time.Duration
	}{
		{"nowhere near a rotation", 40 * time.Minute, RetryInterval},
		{"a rotation just past the retry", RetryInterval + 10*time.Second, RetryInterval},
		{"ten seconds out", 10 * time.Second, 10*time.Second - BurstLead},
		{"one second out, do not overshoot it", time.Second, BurstTick},
		{"the rotation is now", 0, RetryInterval},
		{"the rotation has passed", -5 * time.Minute, RetryInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := waitWhileUnreadable(tt.untilRotation); got != tt.want {
				t.Fatalf("wait %v, want %v", got, tt.want)
			}
			if got := waitWhileUnreadable(tt.untilRotation); got < BurstTick {
				t.Fatalf("wait %v would spin the loop", got)
			}
		})
	}
}
