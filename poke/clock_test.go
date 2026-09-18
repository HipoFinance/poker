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
		sinceFirstSend time.Duration
		untilDeadline  time.Duration
		haveDeadline   bool
		want           time.Duration
	}{
		{
			name: "just sent, stay on the burst cadence",
			// The poke went out at the deadline; keep trying every second in case the block that
			// carried it was produced a moment early.
			pending: true, sinceFirstSend: time.Second, want: BurstTick,
		},
		{
			name:    "the burst has closed, fall back to the minute",
			pending: true, sinceFirstSend: BurstTail + time.Second, want: RetryInterval,
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
			pending: true, sinceFirstSend: 10 * time.Minute,
			untilDeadline: 2 * time.Hour, haveDeadline: true, want: RetryInterval,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := NextWait(tt.pending, tt.sinceFirstSend, tt.untilDeadline, tt.haveDeadline)
			if got != tt.want {
				t.Fatalf("got %v (%v), want %v", got, reason, tt.want)
			}
		})
	}
}

// The burst is only worth its noise if it actually brackets the deadline. Walking a clock through
// it second by second is what proves the loop is awake on both sides.
func TestBurstBracketsTheDeadline(t *testing.T) {
	var c Clock
	base := time.Now().Unix()
	c.Observe(uint32(base))
	deadline := uint32(base + 30)

	awakeAt := map[int64]bool{}
	for offset := int64(-5); offset <= 5; offset++ {
		c.Observe(uint32(base + 30 + offset))
		until := c.Until(deadline)
		pending := offset >= 0 // the poke becomes due at the deadline and is sent from then on
		var since time.Duration
		if pending {
			since = time.Duration(offset) * time.Second
		}
		wait, _ := NextWait(pending, since, until, true)
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
