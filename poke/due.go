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

// refundMargin is how far past distribute's own too_late? threshold this service waits before
// poking a halted treasury's open round.
//
// It exists because participate_until is NOT A GUARD, and this service is otherwise built on the
// assumption that an early send is free. participate_in_election's guards are `state == open` and
// `now() >= min(participate_since, round_since)`, and both run before its accept_message.
// participate_until is read later, inside distribute, after the message has been accepted and
// the state committed:
//
//	int elected?  = ~ config_param(config::next_validators).null?();
//	int too_late? = now() >= min(participate_until, round_since);
//	if elected? | too_late? { ...refund... } else { ...decide_loan_requests -> STAKE... }
//
// Being early at that line does not throw and cost nobody anything. It takes the other branch and
// lends. And `now()` there is the gen_utime of whichever block collated the message, which can
// precede the moment it was sent - which is exactly why the burst starts two seconds early
// everywhere else. Aimed at this boundary the burst is aimed at the wrong side of it: several
// copies go to several endpoints and the deciding value is the minimum gen_utime among them, and
// one early copy is irreversible, because every later copy then throws unable_to_participate.
//
// 300 seconds because that is when config 36 appears anyway: participate_until is
// next_round_since - elections_end_before - 300 (see get_times) and the next
// validator set is published at next_round_since - elections_end_before. So for the upcoming
// round this arm now fires no earlier than the elected? arm, which has no race in either
// direction. What the arm still buys is the stale open round, whose threshold is round_since and
// hours in the past, where the margin is satisfied the moment it is tested and nothing waits.
const refundMargin = 300

// burstGraceSeconds is BurstGrace on the chain's clock, which counts in whole seconds.
const burstGraceSeconds = uint32(BurstGrace / time.Second)

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
	return precise(v, v.participateDue)
}

// DueByContract is what the treasury would accept right now, ignoring this service's own halt
// policy. Confirmation is measured against this and never against what was sent, so that a change
// in policy - the governor halting the pool while a poke is outstanding, say - cannot be mistaken
// for the state having moved.
func DueByContract(v View) []Poke {
	if v.Blind {
		return nil
	}
	return precise(v, v.participateLegal)
}

func precise(v View, participate func(uint32) bool) []Poke {
	var out []Poke
	for _, round := range v.Treasury.Rounds() {
		p := v.Treasury.Participations[round]
		switch p.State {
		case StateOpen:
			if participate(round) {
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

// participateLegal is the contract's guard alone: now() >= min(participate_since, round_since).
func (v View) participateLegal(round uint32) bool {
	return v.Now >= participateAt(v.Treasury.Times.ParticipateSince, round)
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
	if !v.participateLegal(round) {
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
	return v.Now >= v.refundDeadline(round)
}

// refundDeadline is the too_late? arm plus its margin - the earliest moment this service will
// poke a halted treasury's open round.
func (v View) refundDeadline(round uint32) uint32 {
	return participateAt(v.Treasury.Times.ParticipateUntil, round) + refundMargin
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
				// Waiting for distribute's refund branch plus its margin, not for the election
				// window. The other arm of that branch - the next validator set appearing -
				// lands at next_round_since - elections_end_before, which is this same moment on
				// mainnet rather than an earlier one, so this is not an upper bound being
				// approximated. MaxSleep is what would pick it up if a network ever published
				// config 36 sooner.
				at = v.refundDeadline(round)
			}
			if at > v.Now {
				deadlines = append(deadlines, at)
			}
		case StateStaked, StateValidating:
			// Already due if the set has rotated; otherwise it becomes due at the next rotation.
			//
			// This is the one deadline whose event is observed rather than predicted: the poke
			// becomes legal when config 34's hash changes, which happens when the masterchain
			// applies the new set, seconds to a minute after that set's utime_until. So the
			// deadline is kept for BurstGrace PAST utime_until, which holds the loop on its
			// one-second cadence until the rotation is actually seen, instead of dropping it to
			// the 60-second retry at the moment of the rotation. Without this the poker watched
			// the two seconds before the rotation and then looked away.
			if v.Network.CurrentVsetHash != nil && p.CurrentVsetHash != nil &&
				v.Network.CurrentVsetHash.Cmp(p.CurrentVsetHash) == 0 &&
				v.Network.CurrentUntil+burstGraceSeconds > v.Now {
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

// Observe folds in one cycle. `sent` is what actually left for a liteserver this cycle and is the
// only thing that starts a clock: a poke nobody managed to send is not evidence that the treasury
// is refusing anything, and neither is one that a dry run only logged. `dueByContract` is what the
// treasury would still accept, and anything outstanding that has left it has been confirmed - the
// state moved, which is the only confirmation an external ever gets.
//
// Returns the pokes just confirmed.
func (t *Tracker) Observe(dueByContract, sent []Poke, now time.Time) []Poke {
	for _, p := range sent {
		if _, ok := t.first[p]; !ok {
			t.first[p] = now
		}
	}
	current := make(map[Poke]bool, len(dueByContract))
	for _, p := range dueByContract {
		current[p] = true
	}
	var confirmed []Poke
	for p := range t.first {
		if !current[p] {
			confirmed = append(confirmed, p)
			delete(t.first, p)
		}
	}
	sort.Slice(confirmed, func(i, j int) bool { return lessPoke(confirmed[i], confirmed[j]) })
	return confirmed
}

// Reset forgets everything outstanding.
//
// It is deliberately NOT called when blind mode is entered or left. It used to be, and that hid
// wedges: a treasury read failing every few minutes reset every age on each flap, so no poke ever
// reached the alert threshold while PokerBlindMode's `for` never saw five continuous minutes
// either, and a genuinely stuck round raised nothing at all. Blind mode now simply publishes no
// ages - it cannot confirm anything - and the tracker carries across, so an age that was real
// before the outage is still real after it.
func (t *Tracker) Reset() { t.first = map[Poke]time.Time{} }

// Known reports whether this poke has ever left for a liteserver. It is how a cycle tells a
// deadline that has only just arrived from a round that has been retried for ten minutes, which
// decides whether to open a burst.
func (t *Tracker) Known(p Poke) bool {
	_, ok := t.first[p]
	return ok
}

// Newest returns the most recently started poke and how long ago it was first sent. This is what
// the burst cadence is measured against: "did we send something a moment ago", which is the
// minimum age, not the maximum.
func (t *Tracker) Newest(now time.Time) (Poke, time.Duration, bool) {
	var newest Poke
	var at time.Time
	found := false
	for p, sent := range t.first {
		if !found || sent.After(at) || (sent.Equal(at) && lessPoke(p, newest)) {
			newest, at, found = p, sent, true
		}
	}
	if !found {
		return Poke{}, 0, false
	}
	return newest, now.Sub(at), true
}

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

	// failingSince is when the current run of failed reads began, which is not the same as when
	// blind mode began: a transport failure has to persist before it counts.
	failingSince time.Time
}

// BlindTransportGrace is how long every endpoint must fail to return a treasury state before the
// service gives up and pokes blind.
//
// A shape error waits for nothing, because it will read the same way from every endpoint forever.
// A liteserver that cannot answer is a different thing entirely, and on 2026-09-20 one of ours
// answered `CurrentMasterchainInfo` with a block its own shard client had not caught up to yet
// (`is not in db (possibly out of sync: shard_client_seqno=93898063 ls_seqno=93898106)`). That is
// a few seconds of lag. Treated as a shape error it put the service into blind mode twice in ten
// minutes, fired 27 externals each time, and armed the two-hour window in which the halt guard is
// off - for a node that was fine by the next cycle.
const BlindTransportGrace = 10 * time.Minute

// Observe folds in one cycle's read result. It reports whether blind mode was entered or left, so
// the caller logs the transition rather than every cycle.
func (b *blindState) Observe(readErr error, now time.Time) (entered, left bool) {
	if readErr == nil {
		b.failingSince = time.Time{}
		if b.blind {
			b.blind = false
			b.since = time.Time{}
			return false, true
		}
		return false, false
	}

	if b.failingSince.IsZero() {
		b.failingSince = now
	}
	if b.blind {
		return false, false
	}
	if !IsShapeError(readErr) && now.Sub(b.failingSince) < BlindTransportGrace {
		return false, false
	}

	// `since` is now rather than failingSince: the two-hour participate window measures how long
	// this service has been poking without sight, not how long the read has been unhappy.
	b.blind = true
	b.since = now
	return true, false
}

// BlindIn is how much of the grace is left before a failing transport read starts poking blind.
// Zero once blind mode has started, or when no read is currently failing.
func (b *blindState) BlindIn(now time.Time) time.Duration {
	if b.blind || b.failingSince.IsZero() {
		return 0
	}
	left := BlindTransportGrace - now.Sub(b.failingSince)
	if left < 0 {
		return 0
	}
	return left
}

func (b *blindState) Blind() bool      { return b.blind }
func (b *blindState) Since() time.Time { return b.since }
