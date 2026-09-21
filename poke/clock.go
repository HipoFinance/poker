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

	// BurstTail is how long the burst keeps re-sending, measured from its first send rather than
	// from the most recent attempt - otherwise a poke that is refused every second would keep
	// refreshing its own window and never stop.
	//
	// Ten seconds rather than three because an attempt no longer costs a read. It used to be a
	// whole cycle, six liteserver round trips and about a second, which made the real cadence two
	// seconds and fitted two attempts into a three-second tail. The burst now re-sends the set it
	// already decided on without reading anything, so the window is bounded by what is useful
	// rather than by what it costs: ten seconds covers a transition the chain applies several
	// seconds after the computed deadline, which on 2026-09-20 came within four seconds of
	// falling through to the 60-second retry.
	BurstTail = 10 * time.Second

	// BurstTick is the cadence inside the burst window.
	BurstTick = time.Second

	// BurstGrace is how long the one-second cadence continues PAST a deadline whose event has
	// not been observed yet.
	//
	// Some deadlines are a clock this service already tracks - stake_held_until, participate_since
	// - and once they pass the poke is simply due. The validator-set rotation is not: it becomes
	// due when config 34's hash actually changes, which happens when the masterchain applies the
	// new set, and that is seconds to a minute after the set's utime_until. Without a grace the
	// loop bracketed the rotation time itself and then dropped to the 60-second retry at exactly
	// the moment it should have been watching hardest.
	//
	// Observed on mainnet: the rotation landed between 4 and 49 seconds after utime_until across
	// six rounds. Three minutes is well clear of that, and the window is quiet - nothing is due,
	// so nothing is sent, and the logs only speak when something changes.
	BurstGrace = 3 * time.Minute

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
// The rule is that nothing may sleep past something sooner: an approaching deadline and the retry
// interval compete, and the nearer one wins.
//
// It used to carry a third arm, a one-second "burst" cadence while a poke had recently left. That
// moved into Cycle, which now re-sends inside one cycle without reading between attempts - the
// burst is no longer expressed as a short sleep between cycles, and leaving the arm here would be
// dead code that still looked load-bearing. Its one hard-won lesson survives in the move: the
// burst is bounded by the age of the poke's FIRST send, so a wedged round whose poke has been
// outstanding for ten minutes does not re-open a burst every cycle, and does not stop another
// round's deadline getting one either.
func NextWait(pending bool, untilDeadline time.Duration, haveDeadline bool) (time.Duration, string) {
	wait, reason := RetryInterval, "idle"
	if pending {
		reason = "retry"
	}

	if haveDeadline {
		toDeadline := untilDeadline - BurstLead
		if toDeadline < BurstTick {
			// The deadline is here, or within the lead. Wake at the tick and open the burst.
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
