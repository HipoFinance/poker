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
	// within a bounded time even when no deadline is near.
	MaxSleep = 10 * time.Minute
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
//   - While a poke is outstanding and was first sent moments ago, tick every second: this is the
//     tail of the burst, and it is what turns "the deadline passed" into "the transition landed in
//     the first block after it".
//   - While a poke is outstanding but the burst has closed, fall to the plain retry interval.
//   - With no poke outstanding but a deadline within the lead, tick every second so the burst
//     opens on time.
//   - Otherwise sleep until the burst would open, capped.
func NextWait(pending bool, sinceFirstSend time.Duration, untilDeadline time.Duration, haveDeadline bool) (time.Duration, string) {
	switch {
	case pending && sinceFirstSend < BurstTail:
		return BurstTick, "burst"
	case pending:
		return RetryInterval, "retry"
	case haveDeadline && untilDeadline <= BurstLead:
		return BurstTick, "deadline is here"
	case haveDeadline:
		wait := untilDeadline - BurstLead
		if wait > MaxSleep {
			wait = MaxSleep
		}
		if wait < BurstTick {
			wait = BurstTick
		}
		return wait, "next deadline"
	default:
		return RetryInterval, "idle"
	}
}
