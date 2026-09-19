package poke

import (
	"errors"
	"math/big"
	"testing"
	"time"
)

func blindNetwork() NetworkConfig {
	return NetworkConfig{
		CurrentVsetHash: currentHash,
		CurrentSince:    currRound,
		CurrentUntil:    nextRound,
		PreviousSince:   prevRound,
		PreviousUntil:   currRound,
	}
}

// TestCandidatesReachEveryRoundTheTreasuryCanHold pins what blind mode aims at.
//
// The validator sets name only three rounds, but request_loan lets the treasury hold eight
// (treasury.fc:726). A round that missed two rotations, or one left in `held` well past its
// stake_held_until, has a round_since older than config 32 - and those are exactly the incidents
// the spec's Problem section describes. Reaching only as far as the previous set would make blind
// mode unable to see precisely the rounds it exists to rescue, and blind mode has no exit timer:
// it lasts until a human fixes the read.
func TestCandidatesReachEveryRoundTheTreasuryCanHold(t *testing.T) {
	got := blindNetwork().Candidates()

	const epoch = uint32(65536)
	for _, want := range []uint32{prevRound, currRound, nextRound} {
		if !containsRound(got, want) {
			t.Fatalf("candidate set %v is missing the live round %v", got, want)
		}
	}

	// Eight participations is the most the treasury will hold, so the reach has to cover that
	// many rounds back from the newest one.
	for k := uint32(1); k <= 5; k++ {
		want := prevRound - k*epoch
		if !containsRound(got, want) {
			t.Fatalf("candidate set does not reach %v epochs back (missing %v): %v", k+1, want, got)
		}
	}
	if len(got) < 8 {
		t.Fatalf("only %d candidate rounds, fewer than the 8 the treasury can hold: %v", len(got), got)
	}

	// Oldest first, so a round that has been waiting longest is poked first, and no duplicates.
	seen := map[uint32]bool{}
	for i, v := range got {
		if seen[v] {
			t.Fatalf("candidate %v is listed twice: %v", v, got)
		}
		seen[v] = true
		if i > 0 && got[i-1] >= v {
			t.Fatalf("candidates are not oldest first: %v", got)
		}
	}

	// During an election config 36 names the round being elected. It is normally the same value
	// as the current set's utime_until, and must not be listed twice.
	electing := blindNetwork()
	electing.NextSince = nextRound
	if len(electing.Candidates()) != len(got) {
		t.Fatalf("config 36 duplicated a candidate: %v", electing.Candidates())
	}
}

// A chain with no previous validator set, or an unreadable epoch length, must not underflow into
// enormous round numbers or drop the rounds it does know about.
func TestCandidatesDegradeSafely(t *testing.T) {
	young := NetworkConfig{CurrentVsetHash: currentHash, CurrentSince: 100, CurrentUntil: 200}
	for _, v := range young.Candidates() {
		if v == 0 || v > 200 {
			t.Fatalf("a chain younger than the reach produced candidate %v: %v", v, young.Candidates())
		}
	}

	// CurrentUntil == CurrentSince gives a zero epoch; stepping by it would loop on one value.
	flat := NetworkConfig{CurrentVsetHash: currentHash, PreviousSince: 50, CurrentSince: 100, CurrentUntil: 100}
	if got := flat.Candidates(); len(got) != 2 {
		t.Fatalf("a zero epoch produced %v", got)
	}
}

func containsRound(rounds []uint32, want uint32) bool {
	for _, r := range rounds {
		if r == want {
			return true
		}
	}
	return false
}

func TestVsetTimesReadsAValidatorSet(t *testing.T) {
	since, until, err := vsetTimes(vsetCell(t, currRound, nextRound))
	if err != nil {
		t.Fatalf("vsetTimes: %v", err)
	}
	if since != currRound || until != nextRound {
		t.Fatalf("got (%v, %v), want (%v, %v)", since, until, currRound, nextRound)
	}

	// Anything that is not a validator set must be refused rather than read as one, or blind mode
	// would aim at numbers it invented.
	if _, _, err := vsetTimes(participationCell(t, StateHeld, currentHash, 0)); err == nil {
		t.Fatal("a participation cell was read as a validator set")
	}

	// An inverted pair must be refused rather than read. Candidates() subtracts these two to get
	// the epoch length it steps back by, so utime_until <= utime_since underflows uint32 into
	// roughly four billion, passes the `epoch > 0` test, and has blind mode aiming at rounds that
	// never existed.
	for _, bad := range [][2]uint32{{currRound, currRound}, {nextRound, currRound}} {
		if _, _, err := vsetTimes(vsetCell(t, bad[0], bad[1])); err == nil {
			t.Fatalf("a validator set running from %v to %v was accepted", bad[0], bad[1])
		}
	}

	// And the epoch that comes out of a good one is the round length, not something derived.
	n, err := buildNetworkConfig(vsetCell(t, prevRound, currRound), vsetCell(t, currRound, nextRound), nil)
	if err != nil {
		t.Fatalf("buildNetworkConfig: %v", err)
	}
	if got := n.CurrentUntil - n.CurrentSince; got != 65536 {
		t.Fatalf("epoch length is %v, want 65536", got)
	}
}

// TestBlindWindow is the halt guard's fallback. Blind mode cannot read stopped?, so it fires
// participate_in_election for a bounded window and then withdraws it, keeping settlement running
// throughout. Withholding immediately would silently stop the pool lending on any getter change;
// firing forever would let an unrelated upgrade remove the halt guard with nobody told.
func TestBlindWindow(t *testing.T) {
	wall := time.Now()
	candidates := len(blindNetwork().Candidates())

	inside := View{Now: testNow, Network: blindNetwork(), Blind: true, BlindSince: wall.Add(-time.Hour)}
	got := Due(inside, wall)
	if len(got) != candidates*3 {
		t.Fatalf("inside the window got %d pokes, want %d", len(got), candidates*3)
	}
	if !hasOp(got, OpParticipateInElection) {
		t.Fatal("participate_in_election was withheld inside the window")
	}

	outside := View{Now: testNow, Network: blindNetwork(), Blind: true,
		BlindSince: wall.Add(-BlindParticipateWindow - time.Minute)}
	got = Due(outside, wall)
	if len(got) != candidates*2 {
		t.Fatalf("outside the window got %d pokes, want %d", len(got), candidates*2)
	}
	if hasOp(got, OpParticipateInElection) {
		t.Fatal("participate_in_election survived the window: a halted pool could be made to lend indefinitely")
	}
	// Settling must continue forever, however long the read stays broken.
	if !hasOp(got, OpVsetChanged) || !hasOp(got, OpFinishParticipation) {
		t.Fatal("blind mode stopped settling in-flight rounds")
	}
}

// Blind mode has no deadlines to sleep towards: it does not know which rounds exist or what state
// they are in. That is part of the cost of a failed read, and it is why the loop falls back to
// plain retries there rather than bursting forever.
func TestBlindHasNoDeadline(t *testing.T) {
	v := View{Now: testNow, Network: blindNetwork(), Blind: true, BlindSince: time.Now()}
	if d, ok := NextDeadline(v); ok {
		t.Fatalf("blind mode produced a deadline of %v", d)
	}
}

// Blind mode must not consult the treasury state at all, whatever happens to be sitting in the
// view: the whole point is that it has been judged untrustworthy.
func TestBlindIgnoresTheTreasury(t *testing.T) {
	v := View{Now: testNow, Network: blindNetwork(), Blind: true, BlindSince: time.Now()}
	v.Treasury = TreasuryState{
		Stopped:        true,
		Participations: map[uint32]Participation{currRound: {State: StateBurning, CurrentVsetHash: big.NewInt(0)}},
	}
	got := Due(v, time.Now())
	if len(got) != len(blindNetwork().Candidates())*3 {
		t.Fatalf("blind mode read the treasury state: %v", ops(got))
	}
}

func hasOp(pokes []Poke, op Op) bool {
	for _, p := range pokes {
		if p.Op == op {
			return true
		}
	}
	return false
}

// TestBuildNetworkConfigWithoutAnElection is the regression for the bug the first mainnet dry-run
// found: config 36 exists only while an election is open, which is a small part of every round,
// and requesting it alongside the others made tonutils fail the whole read. The service was then
// unable to see anything at all for most of every round and poked nothing.
//
// Two properties matter here. An absent config 36 must produce a complete config rather than an
// error, and the candidate set must not shrink when it goes away - the rounds that can still be
// waiting are named by configs 32 and 34, not by 36.
func TestBuildNetworkConfigWithoutAnElection(t *testing.T) {
	previous := vsetCell(t, prevRound, currRound)
	current := vsetCell(t, currRound, nextRound)

	outside, err := buildNetworkConfig(previous, current, nil)
	if err != nil {
		t.Fatalf("an absent config 36 was treated as a failure: %v", err)
	}
	if outside.CurrentSince != currRound || outside.CurrentUntil != nextRound {
		t.Fatalf("misread the current validator set: %+v", outside)
	}
	if outside.NextSince != 0 {
		t.Fatalf("invented a next validator set: %v", outside.NextSince)
	}
	if !containsRound(outside.Candidates(), currRound) || !containsRound(outside.Candidates(), nextRound) {
		t.Fatalf("an absent config 36 lost a live round: %v", outside.Candidates())
	}

	inside, err := buildNetworkConfig(previous, current, vsetCell(t, nextRound, nextRound+65536))
	if err != nil {
		t.Fatalf("a present config 36 was rejected: %v", err)
	}
	if inside.NextSince != nextRound {
		t.Fatalf("config 36 read as %v, want %v", inside.NextSince, nextRound)
	}

	// The current set is the one thing that must be there.
	if _, err := buildNetworkConfig(previous, nil, nil); err == nil {
		t.Fatal("a missing current validator set was accepted")
	}
}

// TestBlindSinceIsSetOnce is the one property of the blind latch that matters. If a later failure
// refreshed the timestamp, the two-hour participate window would never expire: the poker would keep
// lending a treasury it cannot tell is halted, PokerBlindModeExpired would never fire, and nothing
// else would look wrong.
func TestBlindSinceIsSetOnce(t *testing.T) {
	var b blindState
	start := time.Now()

	if b.Blind() {
		t.Fatal("a fresh latch started blind")
	}
	if entered, left := b.Observe(nil, start); entered || left {
		t.Fatal("a successful read on a healthy latch reported a transition")
	}

	entered, _ := b.Observe(errors.New("tuple is the wrong shape"), start)
	if !entered || !b.Blind() {
		t.Fatal("a failed read did not enter blind mode")
	}
	if !b.Since().Equal(start) {
		t.Fatalf("blind since %v, want %v", b.Since(), start)
	}

	// Every subsequent failure, including ones hours later, must leave the clock alone.
	for _, after := range []time.Duration{time.Minute, time.Hour, 3 * time.Hour} {
		entered, left := b.Observe(errors.New("still wrong"), start.Add(after))
		if entered || left {
			t.Fatalf("a repeat failure after %v reported a transition", after)
		}
		if !b.Since().Equal(start) {
			t.Fatalf("a failure after %v moved the clock to %v; the window would never expire", after, b.Since())
		}
	}

	// And a recovery clears it, so the next outage measures its own window rather than inheriting
	// this one and expiring immediately.
	if _, left := b.Observe(nil, start.Add(4*time.Hour)); !left {
		t.Fatal("a successful read did not leave blind mode")
	}
	if b.Blind() || !b.Since().IsZero() {
		t.Fatalf("leaving blind mode left state behind: blind=%v since=%v", b.Blind(), b.Since())
	}

	later := start.Add(5 * time.Hour)
	b.Observe(errors.New("again"), later)
	if !b.Since().Equal(later) {
		t.Fatalf("a second outage inherited the first one's clock: %v, want %v", b.Since(), later)
	}
}

// TestBlindPastTheWindowStillRetiresOpenRounds. When the participate window closes, blind mode
// stops lending - but it must not stop resolving open rounds, or a round's borrowers keep their
// collateral locked up until somebody notices the alert.
//
// distribute refunds every request whenever the next validator set exists, and config 36 is
// readable without the treasury, so blind mode can tell when participating is safe even though it
// cannot tell whether the treasury is halted.
func TestBlindPastTheWindowStillRetiresOpenRounds(t *testing.T) {
	wall := time.Now()
	expired := wall.Add(-BlindParticipateWindow - time.Minute)

	noElection := View{Now: testNow, Network: blindNetwork(), Blind: true, BlindSince: expired}
	if hasOp(Due(noElection, wall), OpParticipateInElection) {
		t.Fatal("blind mode lent past its window, with no refund branch to fall into")
	}

	electing := blindNetwork()
	electing.NextSince = nextRound
	duringElection := View{Now: testNow, Network: electing, Blind: true, BlindSince: expired}
	got := Due(duringElection, wall)
	if !hasOp(got, OpParticipateInElection) {
		t.Fatal("blind mode left open rounds stranded even though distribute would only refund them")
	}
	// And the settling ops are never withheld, in any mode.
	if !hasOp(got, OpVsetChanged) || !hasOp(got, OpFinishParticipation) {
		t.Fatalf("blind mode stopped settling: %v", got)
	}
}
