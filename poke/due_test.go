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
	tests := []struct {
		name  string
		round uint32
		state State
		hash  *big.Int
		until uint32
		want  uint32
	}{
		{"open waits for the election window", nextRound, StateOpen, currentHash, 0, electionAt},
		{"held waits for stake_held_until", prevRound, StateHeld, currentHash, testNow + 900, testNow + 900},
		{"staked waits for the next rotation", currRound, StateStaked, currentHash, 0, nextRound},
		{"validating waits for the next rotation", currRound, StateValidating, currentHash, 0, nextRound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := trustedView(t, tt.round, tt.state, tt.hash, tt.until, false, electionAt, testNow)
			got, ok := NextDeadline(v)
			if !ok || got != tt.want {
				t.Fatalf("got %v (ok=%v), want %v", got, ok, tt.want)
			}
		})
	}

	// A round that is already due must not contribute a future deadline, or the loop would sleep
	// past the poke it should be sending right now.
	rotated := trustedView(t, currRound, StateStaked, staleHash, 0, false, electionAt, testNow)
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
