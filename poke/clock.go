package poke

import (
	"sync/atomic"
	"time"
)

// Timing constants. See "The first poke is sent at the first opportunity" in
// contract/docs/specs/2026-09-18-poke-service.md.
const (
	// BurstLead is how long before a deadline the burst opens. Sends inside it are expected to
	// be rejected - the guard compares against the block's gen_utime, which is still short of the
	// deadline - and they are sent anyway because a rejected external commits no transaction and
	// costs nobody anything, while being one attempt late costs a whole retry interval.
	BurstLead = 2 * time.Second

	// BurstTail is how long after the first send the one-second cadence continues.
	BurstTail = 3 * time.Second

	// BurstTick is the cadence inside the burst window.
	BurstTick = time.Second

	// RetryInterval is the plain cadence outside it: no jitter and no backoff. Jitter spreads
	// load across many independent senders and there are two of these, and duplicate externals
	// are free for the same reason the burst is.
	RetryInterval = time.Minute

	// MaxSleep caps a wait so that a validator-set rotation or a governance change is picked up
	// within a bounded time even when no deadline is near. It is also the floor under how fast a
	// chain outage can be noticed, because the read clock only advances on a cycle - so
	// PokerReadFailing's threshold is sized as a multiple of this, and the two move together.
	MaxSleep = 5 * time.Minute
)

// Clock converts between the host's clock and the chain's.
//
// Every guard in the treasury compares against now(), which is the gen_utime of the block that
// carries the external, not this process's idea of the time. A host clock a few seconds fast
// fires early on every single deadline and loses a full retry interval to it, every round,
// silently. So the liteserver's own clock is treated as authoritative and the difference is
// carried here.
//
// The offset is a plain integer of seconds rather than a smoothed estimate on purpose: it is
// re-observed on every cycle, and a wrong-but-recent reading is better than a right-but-averaged
// one when the whole point is to hit a specific second.
type Clock struct {
	offset atomic.Int64 // chain seconds minus host seconds
	seen   atomic.Bool
}

// Observe records the chain's current time as reported by a liteserver.
func (c *Clock) Observe(chainNow uint32) {
	c.offset.Store(int64(chainNow) - time.Now().Unix())
	c.seen.Store(true)
}

// Offset is the correction currently applied, in seconds.
func (c *Clock) Offset() int64 { return c.offset.Load() }

// Synced reports whether the chain's clock has ever been observed. Before that the host clock is
// used unchanged, which is right for a first cycle and wrong to rely on afterwards.
func (c *Clock) Synced() bool { return c.seen.Load() }

// Now is the chain's current time.
func (c *Clock) Now() uint32 {
	return uint32(time.Now().Unix() + c.offset.Load())
}

// Until is how long until a chain timestamp, corrected. It can be negative.
func (c *Clock) Until(deadline uint32) time.Duration {
	return time.Duration(int64(deadline)-int64(c.Now())) * time.Second
}

// NextWait decides how long to sleep before the next cycle, and why.
//
// It takes the age of the MOST RECENTLY sent poke, not the oldest. That distinction is the whole
// correctness of this function and it was wrong once: with the oldest age, a single wedged round
// whose poke had been outstanding for ten minutes pushed every cycle into the plain retry
// interval, so a second round whose deadline arrived meanwhile got no burst and waited up to a
// minute. The first-opportunity property switched itself off during exactly the incident it exists
// for.
//
// The rule is that nothing may sleep past something sooner. A just-sent poke wins outright,
// because one second is already the floor; otherwise an approaching deadline and the retry
// interval compete and the nearer one wins.
//
// `sent` is separate from `pending` and is not inferable from the age. `pending` means something
// is due; `sent` means something actually left. Without the distinction an empty tracker reports
// an age of zero, which reads as "sent this instant" and pins the loop at one second forever -
// which is the state when every send is failing, or under DRY_RUN, the two situations where a hot
// loop is least affordable.
func NextWait(pending, sent bool, sinceNewestSend time.Duration, untilDeadline time.Duration, haveDeadline bool) (time.Duration, string) {
	if pending && sent && sinceNewestSend < BurstTail {
		// The tail of a burst: keep trying every second in case the block that carried the last
		// attempt was produced a moment before the deadline. Nothing can be sooner than this.
		return BurstTick, "burst"
	}

	wait, reason := RetryInterval, "idle"
	if pending {
		reason = "retry"
	}

	if haveDeadline {
		toDeadline := untilDeadline - BurstLead
		if toDeadline < BurstTick {
			// The deadline is here, or within the lead. Open the burst.
			toDeadline = BurstTick
		}
		if toDeadline > MaxSleep {
			toDeadline = MaxSleep
		}
		// With nothing outstanding, the next deadline is the only thing worth waking for, so it
		// wins outright and an idle poker sleeps up to MaxSleep. With something outstanding it
		// only wins when it is sooner than the retry, which is what stops a wedged round from
		// sleeping through another round's transition.
		if !pending || toDeadline < wait {
			wait, reason = toDeadline, "next deadline"
		}
	}
	return wait, reason
}
