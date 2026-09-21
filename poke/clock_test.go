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

// TestNextWait pins the cadence between cycles: sleep to the next deadline, or the plain minute.
// The burst itself is no longer a sleep - it runs inside one cycle - and lives in burst_test.go.
func TestNextWait(t *testing.T) {
	tests := []struct {
		name          string
		pending       bool
		untilDeadline time.Duration
		haveDeadline  bool
		want          time.Duration
	}{
		{
			name:    "something outstanding and no deadline known",
			pending: true, want: RetryInterval,
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
			pending:       true,
			untilDeadline: 2 * time.Hour, haveDeadline: true, want: RetryInterval,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := NextWait(tt.pending, tt.untilDeadline, tt.haveDeadline)
			if got != tt.want {
				t.Fatalf("got %v (%v), want %v", got, reason, tt.want)
			}
		})
	}
}

// The lead half of the burst is NextWait's job: the loop has to be awake at the tick before the
// deadline arrives, not discover it a minute late. The tail half runs inside a cycle and is
// covered by TestBurstKeepsItsCadenceAndStops.
func TestTheLoopIsAwakeGoingIntoTheDeadline(t *testing.T) {
	var c Clock
	base := time.Now().Unix()
	c.Observe(uint32(base))
	deadline := uint32(base + 30)

	for offset := int64(-5); offset <= 0; offset++ {
		c.Observe(uint32(base + 30 + offset))
		wait, reason := NextWait(false, c.Until(deadline), true)
		if offset >= -int64(BurstLead/time.Second) {
			if wait != BurstTick {
				t.Fatalf("at deadline%+d the loop slept %v (%v), not the tick", offset, wait, reason)
			}
			continue
		}
		if wait > time.Duration(-offset)*time.Second {
			t.Fatalf("at deadline%+d the loop slept %v (%v), past the deadline", offset, wait, reason)
		}
	}
}

// TestNextWaitDoesNotSleepPastASecondTransition is the regression for the bug that disabled the
// headline feature. A round wedged for ten minutes must not stop the loop from waking for another
// round whose deadline is seconds away.
func TestNextWaitDoesNotSleepPastASecondTransition(t *testing.T) {
	// A poke outstanding for ten minutes - the state PokerPokeUnconfirmed fires on - and another
	// round's transition twenty seconds out.
	wait, reason := NextWait(true, 20*time.Second, true)
	if wait != 20*time.Second-BurstLead {
		t.Fatalf("with a wedged round pending, a deadline %v away produced a %v sleep (%v); "+
			"the second transition would wait for the retry interval", 20*time.Second, wait, reason)
	}

	// The same, with the deadline already inside the lead.
	if wait, _ := NextWait(true, time.Second, true); wait != BurstTick {
		t.Fatalf("an imminent deadline did not open the burst while a poke was outstanding: %v", wait)
	}

	// A far deadline must not drag a pending retry out to MaxSleep.
	if wait, r := NextWait(true, 4*time.Hour, true); wait != RetryInterval {
		t.Fatalf("a pending poke waited %v (%v), want the retry interval", wait, r)
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
