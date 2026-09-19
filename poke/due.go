package poke

import (
	"sort"
	"time"
)

// BlindParticipateWindow is how long blind mode keeps sending participate_in_election.
//
// Blind mode cannot read stopped?, so it cannot honour the halt guard. Withholding participate
// immediately would mean a getter change silently stops the pool lending; firing it forever would
// mean an unrelated upgrade can remove the halt guard with nobody told. So it fires for a bounded
// window, with PokerBlindMode warning from the first minute, and then withdraws.
//
// Two hours is comfortably less than one round, so at most one lending round is exposed, and an
// operator who ignores the warning for two hours gets the conservative behaviour by default.
const BlindParticipateWindow = 2 * time.Hour

// View is one cycle's picture of the world.
type View struct {
	// Now is the chain's time, corrected. Never the host's.
	Now uint32

	Network NetworkConfig

	// Blind is set when the treasury read failed or could not be trusted. Treasury is then
	// meaningless and must not be consulted.
	Blind bool

	// BlindSince is when blind mode started, used only to decide whether the participate window
	// is still open.
	BlindSince time.Time

	Treasury TreasuryState
}

// blindAllowsParticipate reports whether the bounded window is still open.
func (v View) blindAllowsParticipate(now time.Time) bool {
	return now.Sub(v.BlindSince) < BlindParticipateWindow
}

// Due is the set of pokes the chain would accept right now, oldest round first.
//
// In precise mode it mirrors the treasury's own guards, so the normal case sends exactly one
// external and nothing else. In blind mode it mirrors nothing and sends everything plausible,
// because the contract's guards are then the only filter available - and they are a complete one,
// since each runs before accept_message and a rejected external commits no transaction.
func Due(v View, wall time.Time) []Poke {
	if v.Blind {
		return dueBlind(v, wall)
	}
	return duePrecise(v)
}

func dueBlind(v View, wall time.Time) []Poke {
	// Settling never stops: withholding vset_changed or finish_participation would strand
	// in-flight rounds, and neither lends anything.
	ops := []Op{OpVsetChanged, OpFinishParticipation}
	if v.blindAllowsParticipate(wall) || v.refundOnly() {
		ops = AllOps
	}
	var out []Poke
	for _, round := range v.Network.Candidates() {
		for _, op := range ops {
			out = append(out, Poke{Op: op, RoundSince: round})
		}
	}
	return out
}

func duePrecise(v View) []Poke {
	var out []Poke
	for _, round := range v.Treasury.Rounds() {
		p := v.Treasury.Participations[round]
		switch p.State {
		case StateOpen:
			if v.participateDue(round) {
				out = append(out, Poke{Op: OpParticipateInElection, RoundSince: round})
			}
		case StateStaked, StateValidating:
			// vset_changed has no time gate at all: the treasury's guard is that the validator
			// set has actually rotated since this round last looked. Both transitions out of
			// staked and validating use the same op and the same test.
			if v.Network.CurrentVsetHash != nil && p.CurrentVsetHash != nil &&
				v.Network.CurrentVsetHash.Cmp(p.CurrentVsetHash) != 0 {
				out = append(out, Poke{Op: OpVsetChanged, RoundSince: round})
			}
		case StateHeld:
			if v.Now >= p.StakeHeldUntil {
				out = append(out, Poke{Op: OpFinishParticipation, RoundSince: round})
			}
		}
	}
	return out
}

// participateDue is the treasury's own guard for participate_in_election, plus this service's
// halt policy.
//
// The guard is now() >= min(participate_since, round_since). The policy is about what poking an
// open round DOES, which is not the same at every moment:
//
//   - Poked inside the election window, distribute lends the round's requests to the elector.
//   - Poked once the election has closed - `elected? | too_late?` in distribute - it moves every
//     request to `rejected` instead, refunds each borrower's collateral through
//     process_loan_requests, and retires the round without lending a single GRAM.
//
// A halted treasury wants the second and not the first. request_loan checks stopped?, so a halted
// pool can gain no new requests, which means an open round holds only bids placed before the halt
// - a fixed set of third parties whose collateral is stranded for as long as the round stays open.
// Leaving it open indefinitely harms them for no benefit; lending it would put the pool's money
// into the elector for a round and a hold, which is the opposite of what halting was for. Waiting
// for the refund branch resolves the round, returns the collateral and lends nothing.
func (v View) participateDue(round uint32) bool {
	if v.Now < participateAt(v.Treasury.Times.ParticipateSince, round) {
		return false
	}
	if v.Treasury.Stopped {
		return v.refundOnlyFor(round)
	}
	return true
}

// refundOnlyFor reports whether distribute would take its reject-everything branch for this round,
// mirroring:
//
//	int elected?  = ~ config_param(config::next_validators).null?();
//	int too_late? = now() >= min(participate_until, round_since);
//	if elected? | too_late? { ... }
//
// Both arms are read where the contract reads them: config 36's presence, and get_times'
// participate_until against the round's own start.
func (v View) refundOnlyFor(round uint32) bool {
	if v.refundOnly() {
		return true
	}
	return v.Now >= participateAt(v.Treasury.Times.ParticipateUntil, round)
}

// refundOnly is the half of that condition which needs no treasury read: once the next validator
// set exists, distribute refunds whatever it is given, for any round. That is what lets blind mode
// keep retiring stranded open rounds after its participate window has closed, instead of leaving
// their borrowers' collateral locked up until somebody notices.
func (v View) refundOnly() bool {
	return v.Network.NextSince != 0
}

// participateAt is the treasury's own condition: now() >= min(participate_since, round_since).
func participateAt(participateSince, roundSince uint32) uint32 {
	if roundSince < participateSince {
		return roundSince
	}
	return participateSince
}

// NextDeadline is the earliest future moment at which some poke becomes legal, so the loop can
// sleep until it rather than poll towards it.
//
// Blind mode has no deadlines: it does not know which rounds exist or what state they are in, so
// it falls back to the plain retry interval. That is part of the cost of a failed read, and it is
// why blind mode is a degraded mode rather than an equivalent one.
func NextDeadline(v View) (uint32, bool) {
	if v.Blind {
		return 0, false
	}
	var deadlines []uint32
	for _, round := range v.Treasury.Rounds() {
		p := v.Treasury.Participations[round]
		switch p.State {
		case StateOpen:
			if v.participateDue(round) {
				continue // already due; a future deadline would send the loop past it
			}
			at := participateAt(v.Treasury.Times.ParticipateSince, round)
			if v.Treasury.Stopped {
				// Waiting for distribute's refund branch, not the election window. Its other
				// arm - the next validator set appearing - can arrive earlier than this and
				// cannot be predicted, so this is an upper bound and the 10-minute sleep cap is
				// what picks that case up.
				at = participateAt(v.Treasury.Times.ParticipateUntil, round)
			}
			if at > v.Now {
				deadlines = append(deadlines, at)
			}
		case StateStaked, StateValidating:
			// Already due if the set has rotated; otherwise it becomes due at the next rotation,
			// which is the current set's utime_until.
			if v.Network.CurrentVsetHash != nil && p.CurrentVsetHash != nil &&
				v.Network.CurrentVsetHash.Cmp(p.CurrentVsetHash) == 0 &&
				v.Network.CurrentUntil > v.Now {
				deadlines = append(deadlines, v.Network.CurrentUntil)
			}
		case StateHeld:
			if p.StakeHeldUntil > v.Now {
				deadlines = append(deadlines, p.StakeHeldUntil)
			}
		}
	}
	if len(deadlines) == 0 {
		return 0, false
	}
	sort.Slice(deadlines, func(i, j int) bool { return deadlines[i] < deadlines[j] })
	return deadlines[0], true
}

// Tracker remembers when each outstanding poke was first sent, so that "we have been shouting at
// the chain and nothing moved" is expressible.
//
// This is the whole answer to the fact that a successful send proves nothing. An external that
// fails a guard is discarded with no transaction and no receipt, so the only evidence a poke
// worked is that the state stopped asking for it. A poke is therefore outstanding from the moment
// it is first sent until it drops out of the due set.
type Tracker struct {
	first map[Poke]time.Time
}

func NewTracker() *Tracker { return &Tracker{first: map[Poke]time.Time{}} }

// Observe records the current due set and returns the pokes that have just been confirmed - those
// that were outstanding and are no longer due, which means the state moved.
func (t *Tracker) Observe(due []Poke, now time.Time) []Poke {
	current := make(map[Poke]bool, len(due))
	for _, p := range due {
		current[p] = true
		if _, ok := t.first[p]; !ok {
			t.first[p] = now
		}
	}
	var confirmed []Poke
	for p := range t.first {
		if !current[p] {
			confirmed = append(confirmed, p)
			delete(t.first, p)
		}
	}
	sort.Slice(confirmed, func(i, j int) bool {
		if confirmed[i].RoundSince != confirmed[j].RoundSince {
			return confirmed[i].RoundSince < confirmed[j].RoundSince
		}
		return confirmed[i].Op < confirmed[j].Op
	})
	return confirmed
}

// Reset forgets everything outstanding. Called on entering blind mode: there, every op is due for
// every candidate on every cycle and nothing ever leaves the due set, so an age tracked across
// that boundary would grow without bound and report a wedge that is really just a failed read.
// PokerBlindMode is the signal for that, not PokerPokeUnconfirmed.
func (t *Tracker) Reset() { t.first = map[Poke]time.Time{} }

// Oldest returns the longest-outstanding poke and how long it has been outstanding.
func (t *Tracker) Oldest(now time.Time) (Poke, time.Duration, bool) {
	var oldest Poke
	var at time.Time
	found := false
	for p, sent := range t.first {
		if !found || sent.Before(at) || (sent.Equal(at) && lessPoke(p, oldest)) {
			oldest, at, found = p, sent, true
		}
	}
	if !found {
		return Poke{}, 0, false
	}
	return oldest, now.Sub(at), true
}

// Outstanding returns every outstanding poke with its age.
func (t *Tracker) Outstanding(now time.Time) map[Poke]time.Duration {
	out := make(map[Poke]time.Duration, len(t.first))
	for p, sent := range t.first {
		out[p] = now.Sub(sent)
	}
	return out
}

func lessPoke(a, b Poke) bool {
	if a.RoundSince != b.RoundSince {
		return a.RoundSince < b.RoundSince
	}
	return a.Op < b.Op
}

// blindState is the blind-mode latch, kept separate from the chain plumbing so that the one
// property that matters about it can be tested: BlindSince is set ONCE, when blind mode is
// entered, and is not refreshed by the failures that follow.
//
// If it were refreshed, the two-hour participate window would never expire, PokerBlindModeExpired
// would never fire, and the halt guard would be off indefinitely with nobody told - which is the
// exact failure the bounded window exists to prevent. Nothing else would look wrong.
type blindState struct {
	blind bool
	since time.Time
}

// Observe folds in one cycle's read result. It reports whether blind mode was entered or left, so
// the caller logs the transition rather than every cycle.
func (b *blindState) Observe(readErr error, now time.Time) (entered, left bool) {
	switch {
	case readErr != nil && !b.blind:
		b.blind = true
		b.since = now
		return true, false
	case readErr == nil && b.blind:
		b.blind = false
		b.since = time.Time{}
		return false, true
	default:
		return false, false
	}
}

func (b *blindState) Blind() bool      { return b.blind }
func (b *blindState) Since() time.Time { return b.since }
