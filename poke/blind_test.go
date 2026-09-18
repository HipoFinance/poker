package poke

import (
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

// TestCandidatesReachEveryLiveRound pins what blind mode aims at. A round in held or recovering
// carries the round_since of a set that has already rotated, and with an 18-hour round and a
// 9-hour hold that is at most the previous set - so config 32 reaches one full set further back
// than anything that can still be waiting.
func TestCandidatesReachEveryLiveRound(t *testing.T) {
	got := blindNetwork().Candidates()
	want := []uint32{prevRound, currRound, nextRound}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (oldest first)", got, want)
		}
	}

	// During an election config 36 is present and names the round being elected. It is normally
	// the same value as the current set's utime_until, and must not be listed twice.
	electing := blindNetwork()
	electing.NextSince = nextRound
	if got := electing.Candidates(); len(got) != 3 {
		t.Fatalf("config 36 duplicated a candidate: %v", got)
	}

	// Outside the election window config 36 is absent, which is normal and not a gap.
	absent := blindNetwork()
	absent.NextSince = 0
	if got := absent.Candidates(); len(got) != 3 {
		t.Fatalf("an absent config 36 changed the candidate set: %v", got)
	}
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

	outside, err := buildNetworkConfig(nil, previous, current, nil)
	if err != nil {
		t.Fatalf("an absent config 36 was treated as a failure: %v", err)
	}
	if outside.CurrentSince != currRound || outside.CurrentUntil != nextRound {
		t.Fatalf("misread the current validator set: %+v", outside)
	}
	if outside.NextSince != 0 {
		t.Fatalf("invented a next validator set: %v", outside.NextSince)
	}
	if got := len(outside.Candidates()); got != 3 {
		t.Fatalf("outside an election there are %d candidate rounds, want 3", got)
	}

	inside, err := buildNetworkConfig(nil, previous, current, vsetCell(t, nextRound, nextRound+65536))
	if err != nil {
		t.Fatalf("a present config 36 was rejected: %v", err)
	}
	if inside.NextSince != nextRound {
		t.Fatalf("config 36 read as %v, want %v", inside.NextSince, nextRound)
	}

	// The current set is the one thing that must be there.
	if _, err := buildNetworkConfig(nil, previous, nil, nil); err == nil {
		t.Fatal("a missing current validator set was accepted")
	}
}
