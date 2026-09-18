package poke

import (
	"encoding/hex"
	"testing"
	"time"
)

// TestBodyMatchesTheWrapper pins the encoding against the contract repository's own sender.
//
// The expected values were produced by wrappers/Treasury.ts's sendParticipateInElection,
// sendVsetChanged and sendFinishParticipation - via @ton/core, with the op-codes imported from
// wrappers/common.ts - for query_id and round_since both 1788104456. Two independent senders of
// the same three messages is exactly the situation where a silent divergence is possible and
// would be found only by a round that failed to advance, so it is pinned here rather than
// assumed.
func TestBodyMatchesTheWrapper(t *testing.T) {
	const queryID, roundSince = 1788104456, 1788104456

	tests := []struct {
		op   Op
		want string
	}{
		{OpParticipateInElection, "b5ee9c72410101010012000020574a297b000000006a944f086a944f082e2c93da"},
		{OpVsetChanged, "b5ee9c724101010100120000202f0b5b3b000000006a944f086a944f08c68c99de"},
		{OpFinishParticipation, "b5ee9c7241010101001200002023274435000000006a944f086a944f08ab744280"},
	}
	for _, tt := range tests {
		t.Run(tt.op.String(), func(t *testing.T) {
			body := Body(Poke{Op: tt.op, RoundSince: roundSince}, queryID)
			got := hex.EncodeToString(body.ToBOC())
			if got != tt.want {
				t.Fatalf("body is\n  %v\nbut wrappers/Treasury.ts builds\n  %v", got, tt.want)
			}
		})
	}
}

// TestQueryIDVariesPerAttempt. A deterministic id would read better in an explorer and would let
// duplicate sends collapse to one message hash, but nodes cache the hashes of externals they have
// already processed - and the attempt that matters most is the one a second or two after an
// attempt that was correctly rejected for being early. A stable hash risks that retry being
// dropped as a duplicate at exactly the deadline it exists to hit.
func TestQueryIDVariesPerAttempt(t *testing.T) {
	first := QueryID(uint32(testNow))
	second := QueryID(uint32(testNow + 1))
	if first == second {
		t.Fatal("two attempts a second apart would produce the same message hash")
	}

	p := Poke{Op: OpFinishParticipation, RoundSince: prevRound}
	if string(Body(p, first).Hash()) == string(Body(p, second).Hash()) {
		t.Fatal("two attempts a second apart hash identically")
	}
}

// TestTracker is the answer to "a successful send proves nothing". An external that fails a guard
// is discarded with no transaction and no receipt, so the only evidence a poke worked is that the
// state stopped asking for it.
func TestTracker(t *testing.T) {
	tr := NewTracker()
	start := time.Now()
	poke := Poke{Op: OpFinishParticipation, RoundSince: prevRound}

	if confirmed := tr.Observe([]Poke{poke}, start); len(confirmed) != 0 {
		t.Fatalf("a freshly sent poke was reported as confirmed: %v", confirmed)
	}

	// Still due a minute later: the send went through and nothing happened. This is the shape of
	// the incident the whole service exists for, and it has to be visible.
	later := start.Add(time.Minute)
	if confirmed := tr.Observe([]Poke{poke}, later); len(confirmed) != 0 {
		t.Fatalf("an unacknowledged poke was reported as confirmed: %v", confirmed)
	}
	got, age, ok := tr.Oldest(later)
	if !ok || got != poke || age != time.Minute {
		t.Fatalf("oldest is %v aged %v (ok=%v), want %v aged 1m", got, age, ok, poke)
	}
	if len(tr.Outstanding(later)) != 1 {
		t.Fatalf("outstanding set is %v", tr.Outstanding(later))
	}

	// The state moved: the poke is no longer due, which is the only confirmation there is.
	confirmed := tr.Observe(nil, later.Add(time.Second))
	if len(confirmed) != 1 || confirmed[0] != poke {
		t.Fatalf("the transition was not detected: %v", confirmed)
	}
	if _, _, ok := tr.Oldest(later.Add(time.Second)); ok {
		t.Fatal("a confirmed poke stayed outstanding")
	}
	if len(tr.Outstanding(later)) != 0 {
		t.Fatal("a confirmed poke kept its series alive")
	}
}

// Reset is what keeps blind mode from reporting a wedge. There every op is due for every
// candidate on every cycle, so nothing ever leaves the due set and an age carried across that
// boundary would grow without bound.
func TestTrackerResetOnBlindBoundary(t *testing.T) {
	tr := NewTracker()
	start := time.Now()
	tr.Observe([]Poke{{Op: OpVsetChanged, RoundSince: currRound}}, start)
	tr.Reset()
	if _, _, ok := tr.Oldest(start.Add(time.Hour)); ok {
		t.Fatal("an age survived the blind-mode boundary and would read as a wedge")
	}
}
