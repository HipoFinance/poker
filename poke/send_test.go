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
	due := []Poke{poke}

	if confirmed := tr.Observe(due, due, start); len(confirmed) != 0 {
		t.Fatalf("a freshly sent poke was reported as confirmed: %v", confirmed)
	}

	// Still due a minute later: the send went through and nothing happened. This is the shape of
	// the incident the whole service exists for, and it has to be visible.
	later := start.Add(time.Minute)
	if confirmed := tr.Observe(due, due, later); len(confirmed) != 0 {
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
	confirmed := tr.Observe(nil, nil, later.Add(time.Second))
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

// Only what actually left starts a clock. A poke no liteserver accepted - or one a dry run merely
// logged - is not evidence that the treasury is refusing anything, and counting it as outstanding
// pages someone about a wedged round when the real fault is this host's network path.
func TestTrackerIgnoresWhatWasNotSent(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	due := []Poke{{Op: OpParticipateInElection, RoundSince: nextRound}}

	tr.Observe(due, nil, now)
	if _, _, ok := tr.Oldest(now.Add(time.Hour)); ok {
		t.Fatal("a poke that was never sent became outstanding")
	}
	if len(tr.Outstanding(now.Add(time.Hour))) != 0 {
		t.Fatal("a poke that was never sent was published")
	}

	// And once it is sent, the clock starts from then rather than from when it first became due.
	tr.Observe(due, due, now.Add(time.Hour))
	if _, age, ok := tr.Oldest(now.Add(time.Hour)); !ok || age != 0 {
		t.Fatalf("the clock did not start at the send: age %v (ok=%v)", age, ok)
	}
}

// Confirmation is measured against the treasury's own guards, not against what this service chose
// to send. Otherwise the governor halting the pool mid-window - which makes participate stop being
// due by policy - would be logged and counted as a state transition that never happened.
func TestTrackerDoesNotConfirmOnAPolicyChange(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	poke := Poke{Op: OpParticipateInElection, RoundSince: nextRound}

	tr.Observe([]Poke{poke}, []Poke{poke}, now)

	// The treasury would still accept it; only this service has decided to hold off.
	confirmed := tr.Observe([]Poke{poke}, nil, now.Add(time.Minute))
	if len(confirmed) != 0 {
		t.Fatalf("a policy change was counted as a transition: %v", confirmed)
	}
	if _, age, ok := tr.Oldest(now.Add(time.Minute)); !ok || age != time.Minute {
		t.Fatalf("the poke stopped being outstanding: age %v (ok=%v)", age, ok)
	}
}

// Newest is what the burst cadence is measured against. Oldest is what the alert is measured
// against. Using the oldest for both is what let a single wedged round switch off the burst for
// every other round.
func TestTrackerNewestAndOldest(t *testing.T) {
	tr := NewTracker()
	start := time.Now()
	wedged := Poke{Op: OpParticipateInElection, RoundSince: nextRound}
	fresh := Poke{Op: OpFinishParticipation, RoundSince: prevRound}

	tr.Observe([]Poke{wedged}, []Poke{wedged}, start)
	later := start.Add(10 * time.Minute)
	tr.Observe([]Poke{wedged, fresh}, []Poke{fresh}, later)

	if p, age, _ := tr.Oldest(later); p != wedged || age != 10*time.Minute {
		t.Fatalf("oldest is %v aged %v, want the wedged poke aged 10m", p, age)
	}
	if p, age, _ := tr.Newest(later); p != fresh || age != 0 {
		t.Fatalf("newest is %v aged %v, want the fresh poke aged 0", p, age)
	}
}

// Reset must not be wired to the blind-mode boundary. It used to be, and a treasury read failing
// every few minutes then reset every age on each flap - so no poke ever reached the alert
// threshold while PokerBlindMode never saw five continuous minutes either, and a genuinely stuck
// round raised nothing at all.
func TestTrackerSurvivesABlindFlap(t *testing.T) {
	tr := NewTracker()
	start := time.Now()
	poke := Poke{Op: OpFinishParticipation, RoundSince: prevRound}
	due := []Poke{poke}

	tr.Observe(due, due, start)
	// Three blind cycles in between, during which nothing is observed and nothing is confirmed.
	later := start.Add(4 * time.Minute)
	tr.Observe(due, due, later)

	if _, age, ok := tr.Oldest(later); !ok || age != 4*time.Minute {
		t.Fatalf("age is %v (ok=%v) after a blind flap, want 4m; a wedge would be invisible", age, ok)
	}
}
