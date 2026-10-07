package poke

import (
	"math/big"
	"testing"
	"time"

	"github.com/xssnick/tonutils-go/tvm/cell"
)

// A realistic moment on mainnet: one round validating, an older one settling, and the next round
// open for requests. The numbers are one 65536-second validation round apart, as they are on
// chain, because the relationship between them is what several of these tests are about.
const (
	prevRound  = uint32(1788104456) // rotated out; the round that is held or recovering
	currRound  = uint32(1788169992) // prevRound + 65536; the round validating now
	nextRound  = uint32(1788235528) // currRound + 65536; the open round, and the next rotation
	testNow    = uint32(1788200000) // part way through currRound
	electionAt = uint32(1788226528) // participate_since for nextRound
)

var (
	currentHash = big.NewInt(0xABCDEF)
	staleHash   = big.NewInt(0x123456)
)

// trustedView builds a view with a trusted treasury read, holding exactly one round.
func trustedView(t *testing.T, round uint32, state State, vsetHash *big.Int, stakeHeldUntil uint32, stopped bool, participateSince, now uint32) View {
	t.Helper()
	rounds := map[uint32]*cell.Cell{
		round: participationCell(t, state, vsetHash, stakeHeldUntil),
	}
	ts, err := parseTreasuryState(stateTuple(participationsDict(t, rounds), boolInt(stopped)))
	if err != nil {
		t.Fatalf("parseTreasuryState: %v", err)
	}
	ts.Times, err = parseTimes(timesTuple(currRound, participateSince, participateSince+600, nextRound, nextRound+65536, 32768))
	if err != nil {
		t.Fatalf("parseTimes: %v", err)
	}
	return View{
		Now: now,
		Network: NetworkConfig{
			CurrentVsetHash: currentHash,
			CurrentSince:    currRound,
			CurrentUntil:    nextRound,
			PreviousSince:   prevRound,
			PreviousUntil:   currRound,
		},
		Treasury: ts,
	}
}

func boolInt(b bool) int64 {
	if b {
		return -1 // FunC true
	}
	return 0
}

func ops(pokes []Poke) []Op {
	out := make([]Op, 0, len(pokes))
	for _, p := range pokes {
		out = append(out, p.Op)
	}
	return out
}

func sameOps(got []Poke, want []Op) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Op != want[i] {
			return false
		}
	}
	return true
}

// TestDueByState walks every participation state and pins which op, if any, the treasury would
// accept for it. These are the contract's own guards restated; if treasury.fc's state machine
// changes, this is the test that should fail.
func TestDueByState(t *testing.T) {
	tests := []struct {
		name  string
		round uint32
		state State
		hash  *big.Int
		until uint32
		want  []Op
	}{
		{"open, election window reached", nextRound, StateOpen, currentHash, 0, []Op{OpParticipateInElection}},
		{"distributing is driven on chain", nextRound, StateDistributing, currentHash, 0, nil},
		{"staked, the set has rotated", currRound, StateStaked, staleHash, 0, []Op{OpVsetChanged}},
		{"staked, the set has not rotated", currRound, StateStaked, currentHash, 0, nil},
		{"validating, the set has rotated", prevRound, StateValidating, staleHash, 0, []Op{OpVsetChanged}},
		{"validating, the set has not rotated", currRound, StateValidating, currentHash, 0, nil},
		{"held, stake_held_until passed", prevRound, StateHeld, currentHash, testNow - 1, []Op{OpFinishParticipation}},
		{"held, still frozen", prevRound, StateHeld, currentHash, testNow + 3600, nil},
		{"recovering is driven on chain", prevRound, StateRecovering, currentHash, testNow - 1, nil},
		{"ready_to_burn waits for an older round", prevRound, StateReadyToBurn, currentHash, testNow - 1, nil},
		{"burning is driven by the collection", prevRound, StateBurning, currentHash, testNow - 1, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// participate_since in the past, so the open case turns on the state rather than the
			// clock; the clock is TestParticipateIsNotDueEarly's subject.
			v := trustedView(t, tt.round, tt.state, tt.hash, tt.until, false, testNow-600, testNow)
			got := Due(v, time.Now())
			if !sameOps(got, tt.want) {
				t.Fatalf("got %v, want %v", ops(got), tt.want)
			}
			for _, p := range got {
				if p.RoundSince != tt.round {
					t.Fatalf("poked round %v, want %v", p.RoundSince, tt.round)
				}
			}
		})
	}
}

// TestParticipateIsNotDueEarly pins the treasury's own condition,
// now() >= min(participate_since, round_since). Both arms matter: normally the election window
// opens first, but a round whose own start has already passed - a round that missed its election
// entirely - is legal to participate in immediately, and poking it is how its borrowers get their
// collateral back rather than leaving it open forever.
func TestParticipateIsNotDueEarly(t *testing.T) {
	early := trustedView(t, nextRound, StateOpen, currentHash, 0, false, electionAt, testNow)
	if got := Due(early, time.Now()); len(got) != 0 {
		t.Fatalf("poked %v before the election window opened at %v: %v", testNow, electionAt, ops(got))
	}

	onTime := trustedView(t, nextRound, StateOpen, currentHash, 0, false, electionAt, electionAt)
	if got := Due(onTime, time.Now()); !sameOps(got, []Op{OpParticipateInElection}) {
		t.Fatalf("did not poke at participate_since: %v", ops(got))
	}

	// The round_since arm: a stranded open round whose start has passed, with an election window
	// still notionally in the future.
	stranded := trustedView(t, prevRound, StateOpen, currentHash, 0, false, electionAt, testNow)
	if got := Due(stranded, time.Now()); !sameOps(got, []Op{OpParticipateInElection}) {
		t.Fatalf("a stranded open round was left unpoked: %v", ops(got))
	}
}

// TestStoppedDefersParticipateToTheRefundBranch is the halt policy.
//
// request_loan checks stopped?, so a halted pool can gain no new requests and an open round holds
// only bids placed before the halt. Leaving it open strands that collateral; lending it puts the
// pool's money in the elector for a round and a hold, right after the governor halted it.
// distribute's `elected? | too_late?` branch is the way out: it refunds every request and retires
// the round without lending. So while stopped, participate waits for that branch and settling
// carries on untouched.
func TestStoppedDefersParticipateToTheRefundBranch(t *testing.T) {
	// participate_until is participate_since + 600, and the service waits refundMargin past it,
	// because participate_until is a branch inside distribute rather than a guard in front of it.
	const refundAt = electionAt + 600 + refundMargin

	inWindow := trustedView(t, nextRound, StateOpen, currentHash, 0, true, electionAt, electionAt+1)
	if got := Due(inWindow, time.Now()); len(got) != 0 {
		t.Fatalf("a halted treasury was poked inside the election window, which lends: %v", ops(got))
	}

	// The margin is the whole safety of this path, so it is pinned right at the boundary. A send
	// one second before distribute's threshold is not free the way an early send is everywhere
	// else in this service: participate_until is read after accept_message, so early means the
	// round is STAKED, not that the message is thrown away.
	for _, early := range []uint32{electionAt + 600 - 1, electionAt + 600, refundAt - 1} {
		v := trustedView(t, nextRound, StateOpen, currentHash, 0, true, electionAt, early)
		if got := Due(v, time.Now()); len(got) != 0 {
			t.Fatalf("a halted treasury was poked at %v, %vs before the refund branch is safe: %v",
				early, refundAt-early, ops(got))
		}
	}

	afterWindow := trustedView(t, nextRound, StateOpen, currentHash, 0, true, electionAt, refundAt)
	if got := Due(afterWindow, time.Now()); !sameOps(got, []Op{OpParticipateInElection}) {
		t.Fatalf("a halted treasury left its open round stranded past participate_until: %v", ops(got))
	}

	// The other arm of distribute's condition: once the next validator set exists, distribute
	// refunds whatever it is given, whatever the clock says.
	elected := trustedView(t, nextRound, StateOpen, currentHash, 0, true, electionAt, electionAt+1)
	elected.Network.NextSince = nextRound
	if got := Due(elected, time.Now()); !sameOps(got, []Op{OpParticipateInElection}) {
		t.Fatalf("the elected? arm of the refund branch was not used: %v", ops(got))
	}

	// A stale open round - one whose own start has passed - is past min(participate_until,
	// round_since) by definition, so its borrowers get their collateral back immediately.
	stale := trustedView(t, prevRound, StateOpen, currentHash, 0, true, electionAt, testNow)
	if got := Due(stale, time.Now()); !sameOps(got, []Op{OpParticipateInElection}) {
		t.Fatalf("a stale open round stayed stranded on a halted treasury: %v", ops(got))
	}
}

// Settling must be completely unaffected by a halt, or in-flight rounds never finish and their
// unstake bills are never paid.
func TestStoppedStillSettles(t *testing.T) {
	held := trustedView(t, prevRound, StateHeld, currentHash, testNow-1, true, electionAt, testNow)
	if got := Due(held, time.Now()); !sameOps(got, []Op{OpFinishParticipation}) {
		t.Fatalf("a stopped treasury stopped settling: %v", ops(got))
	}

	staked := trustedView(t, currRound, StateStaked, staleHash, 0, true, electionAt, testNow)
	if got := Due(staked, time.Now()); !sameOps(got, []Op{OpVsetChanged}) {
		t.Fatalf("a stopped treasury stopped observing the validator set: %v", ops(got))
	}
}

// An unhalted treasury must still lend at the normal moment, or the pool simply stops earning.
func TestNotStoppedParticipatesInTheElectionWindow(t *testing.T) {
	v := trustedView(t, nextRound, StateOpen, currentHash, 0, false, electionAt, electionAt)
	if got := Due(v, time.Now()); !sameOps(got, []Op{OpParticipateInElection}) {
		t.Fatalf("a healthy treasury did not lend at participate_since: %v", ops(got))
	}
}

// TestNextDeadline pins what the loop sleeps until. This is what makes the first poke land in the
// first block after a transition becomes legal rather than up to a minute later.
func TestNextDeadline(t *testing.T) {
	// The rotation cases are read after the election window has closed. Before it opens, a view
	// holding no round for the next election has the window's opening as an earlier deadline -
	// see TestTheElectionWindowIsADeadlineBeforeAnyoneBids - and these are about the rotation.
	afterWindow := electionAt + 700

	tests := []struct {
		name  string
		round uint32
		state State
		hash  *big.Int
		until uint32
		now   uint32
		want  uint32
	}{
		{"open waits for the election window", nextRound, StateOpen, currentHash, 0, testNow, electionAt},
		{"held waits for stake_held_until", prevRound, StateHeld, currentHash, testNow + 900, testNow, testNow + 900},
		{"staked waits for the next rotation", currRound, StateStaked, currentHash, 0, afterWindow, nextRound},
		{"validating waits for the next rotation", currRound, StateValidating, currentHash, 0, afterWindow, nextRound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := trustedView(t, tt.round, tt.state, tt.hash, tt.until, false, electionAt, tt.now)
			got, ok := NextDeadline(v)
			if !ok || got != tt.want {
				t.Fatalf("got %v (ok=%v), want %v", got, ok, tt.want)
			}
		})
	}

	// A round that is already due must not contribute a future deadline, or the loop would sleep
	// past the poke it should be sending right now.
	rotated := trustedView(t, currRound, StateStaked, staleHash, 0, false, electionAt, afterWindow)
	if d, ok := NextDeadline(rotated); ok {
		t.Fatalf("an already-due round produced a future deadline of %v", d)
	}

	// A stopped treasury waits for the refund branch, not the election window, so its deadline
	// is participate_until rather than participate_since.
	stopped := trustedView(t, nextRound, StateOpen, currentHash, 0, true, electionAt, testNow)
	d, ok := NextDeadline(stopped)
	want := electionAt + 600 + refundMargin
	if !ok || d != want {
		t.Fatalf("a halted treasury scheduled %v (ok=%v), want the refund branch plus its margin at %v", d, ok, want)
	}
}

// TestRotationIsWatchedPastItsDeadline replays what happened on mainnet at 2026-09-19T08:59:52Z.
//
// A validator-set rotation is the one deadline whose event is observed rather than predicted: the
// poke becomes legal when config 34's hash changes, and the masterchain applies the new set some
// seconds after that set's utime_until. The loop used to keep the deadline only while it was in
// the future, so at the rotation second itself - with the config not yet rotated and therefore
// nothing due - it dropped to the 60-second retry and looked away. A borrower sent both messages
// 49 seconds later and the poker woke to find the work already done.
func TestRotationIsWatchedPastItsDeadline(t *testing.T) {
	// currRound is validating and the set has not rotated yet, so nothing is due.
	atRotation := trustedView(t, currRound, StateValidating, currentHash, 0, false, electionAt, nextRound)
	if got := Due(atRotation, time.Now()); len(got) != 0 {
		t.Fatalf("something was due before the config rotated: %v", ops(got))
	}

	// The deadline must survive the moment it names, and for a good while after it.
	for _, after := range []uint32{0, 1, 30, 49, 120} {
		v := trustedView(t, currRound, StateValidating, currentHash, 0, false, electionAt, nextRound+after)
		d, ok := NextDeadline(v)
		if !ok || d != nextRound {
			t.Fatalf("%vs past the rotation the loop had no deadline (got %v, ok=%v), so it would "+
				"sleep for a minute while waiting for a config change", after, d, ok)
		}
		wait, _ := NextWait(false, time.Duration(int64(d)-int64(v.Now))*time.Second, true)
		if wait != BurstTick {
			t.Fatalf("%vs past the rotation the loop waits %v, not the burst cadence", after, wait)
		}
	}

	// It must not watch forever, or a chain that stops rotating pins the loop at 1Hz.
	stale := trustedView(t, currRound, StateValidating, currentHash, 0, false, electionAt, nextRound+burstGraceSeconds+1)
	if d, ok := NextDeadline(stale); ok {
		t.Fatalf("the rotation grace never closed: still watching %v", d)
	}

	// And once the set has actually rotated the poke is due, so there is nothing left to watch.
	rotated := trustedView(t, currRound, StateValidating, staleHash, 0, false, electionAt, nextRound+10)
	if got := Due(rotated, time.Now()); !sameOps(got, []Op{OpVsetChanged}) {
		t.Fatalf("after the rotation, got %v", ops(got))
	}
	if d, ok := NextDeadline(rotated); ok {
		t.Fatalf("a due poke also produced a deadline of %v", d)
	}
}

// TestTheElectionWindowIsADeadlineBeforeAnyoneBids. A participation exists only once the first
// request_loan arrives, and every other deadline is read off a participation - so with nobody
// having bid, the window's opening was invisible and the loop slept through it.
//
// 2026-10-07, round 1791381256: both borrower hosts broadcast their sealed bid one second before
// the window, the first request landed at 11:22:43, the window opened at 11:22:44, and this service
// - last awake at 11:20:15 with nothing to aim at - woke at 11:25:15 to find the round already
// staked by a poke sent from somewhere else at 11:24:42.
func TestTheElectionWindowIsADeadlineBeforeAnyoneBids(t *testing.T) {
	// The steady state between rounds: one round validating, nothing on the books for the next.
	noBidsYet := func(now uint32) View {
		return trustedView(t, currRound, StateValidating, currentHash, 0, false, electionAt, now)
	}

	got, ok := NextDeadline(noBidsYet(testNow))
	if !ok || got != electionAt {
		t.Fatalf("with no round for the next election the deadline is %v (ok=%v), want the window opening at %v",
			got, ok, electionAt)
	}

	// The reading from the day: 149 seconds before the window. It slept five minutes.
	v := noBidsYet(electionAt - 149)
	d, _ := NextDeadline(v)
	wait, reason := NextWait(false, time.Duration(int64(d)-int64(v.Now))*time.Second, true)
	if wait != 149*time.Second-BurstLead {
		t.Fatalf("149s before the window the loop sleeps %v (%v); it has to wake at the lead, not sleep through", wait, reason)
	}
}

// From the lead until the grace runs out the loop has to be reading every tick, because a round
// created a second before the window is not visible to a read until a masterchain block has
// referenced the shard block it landed in.
func TestTheWindowOpeningIsWatchedUntilTheRoundAppears(t *testing.T) {
	for _, offset := range []int64{-2, -1, 0, 1, 5, 15, int64(participateGraceSeconds) - 1} {
		now := uint32(int64(electionAt) + offset)
		v := trustedView(t, currRound, StateValidating, currentHash, 0, false, electionAt, now)
		d, ok := NextDeadline(v)
		if !ok || d != electionAt {
			t.Fatalf("at window%+d the deadline is %v (ok=%v), want the opening still held at %v", offset, d, ok, electionAt)
		}
		if wait, _ := NextWait(false, time.Duration(int64(d)-int64(now))*time.Second, true); wait != BurstTick {
			t.Fatalf("at window%+d the loop waits %v, not a tick; a round created at the last second is found late", offset, wait)
		}
	}
}

// Past the grace the tick cadence has to stop - a round nobody bids on must not be read every
// second for ten minutes - but a late bid should still wait a minute, not MaxSleep.
func TestTheRestOfTheWindowIsPolledAtTheRetryInterval(t *testing.T) {
	for _, offset := range []uint32{participateGraceSeconds, participateGraceSeconds + 60, 599} {
		now := electionAt + offset
		v := trustedView(t, currRound, StateValidating, currentHash, 0, false, electionAt, now)
		d, ok := NextDeadline(v)
		if !ok {
			t.Fatalf("%vs into the window there is no deadline at all; the loop sleeps MaxSleep", offset)
		}
		wait, _ := NextWait(false, time.Duration(int64(d)-int64(now))*time.Second, true)
		if wait <= BurstTick {
			t.Fatalf("%vs into the window the loop is still reading every tick", offset)
		}
		if wait > RetryInterval {
			t.Fatalf("%vs into the window the loop sleeps %v; a late bid would wait that long", offset, wait)
		}
	}

	// Once the window has closed there is nothing to watch for, and the rotation is next.
	closed := trustedView(t, currRound, StateValidating, currentHash, 0, false, electionAt, electionAt+600)
	if d, ok := NextDeadline(closed); !ok || d != nextRound {
		t.Fatalf("after the window closed the deadline is %v (ok=%v), want the rotation at %v", d, ok, nextRound)
	}
}

// The watch is for a round that is NOT on the books. Once it is there, in any state, its own
// state speaks for it - and a halted treasury accepts no request, so no round can open at all.
func TestTheWindowIsNotWatchedWhenThereIsNothingToWaitFor(t *testing.T) {
	inWindow := electionAt + 5

	// Someone has already bid and the round has already been driven on.
	for _, state := range []State{StateDistributing, StateStaked} {
		v := trustedView(t, nextRound, state, currentHash, 0, false, electionAt, inWindow)
		if d, ok := NextDeadline(v); ok && d == electionAt {
			t.Fatalf("a round already %v was still being watched for at the window opening", state)
		}
	}

	// request_loan throws err::stopped, so nothing can open.
	halted := trustedView(t, currRound, StateValidating, currentHash, 0, true, electionAt, electionAt-10)
	if d, ok := NextDeadline(halted); ok && d == electionAt {
		t.Fatal("a halted treasury, which accepts no loan request, was watched for a round opening")
	}

	// Blind mode knows nothing about rounds or times.
	blind := trustedView(t, currRound, StateValidating, currentHash, 0, false, electionAt, electionAt-10)
	blind.Blind = true
	if _, ok := NextDeadline(blind); ok {
		t.Fatal("blind mode produced a deadline")
	}

	// And times that make no sense are not acted on.
	broken := trustedView(t, currRound, StateValidating, currentHash, 0, false, electionAt, electionAt-10)
	broken.Treasury.Times.ParticipateUntil = broken.Treasury.Times.ParticipateSince
	if d, ok := NextDeadline(broken); ok && d == electionAt {
		t.Fatal("a window that closes when it opens was watched")
	}
}

// And the point of all of it: once a read does see the round, the poke is due at once.
func TestARoundThatAppearsInsideTheWindowIsPokedAtOnce(t *testing.T) {
	appeared := trustedView(t, nextRound, StateOpen, currentHash, 0, false, electionAt, electionAt+4)
	if got := Due(appeared, time.Now()); !sameOps(got, []Op{OpParticipateInElection}) {
		t.Fatalf("a round that opened four seconds into the window is due %v, want participate_in_election", ops(got))
	}
}

// The same thing through schedule, which is what Cycle calls: the deadline has to survive the trip
// through the clock, or the pieces above are each right and the loop still sleeps five minutes.
func TestScheduleWakesForAWindowWithNoRound(t *testing.T) {
	p := &Poker{clock: &Clock{}, tracker: NewTracker()}
	now := electionAt - 10
	p.clock.Observe(now)

	v := trustedView(t, currRound, StateValidating, currentHash, 0, false, electionAt, now)
	wait, reason := p.schedule(v, false)
	if wait > 10*time.Second {
		t.Fatalf("ten seconds before a window nobody has bid in yet, the loop sleeps %v (%v)", wait, reason)
	}
	if wait < BurstTick {
		t.Fatalf("the loop would spin: %v", wait)
	}
}
