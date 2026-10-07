package poke

import (
	"context"
	"testing"
)

// TestWhichRefusalsMeanTheRoundMovedPast. Narrower than Settled on purpose: an unrecognised code
// stops a burst, but it does not say the state went forwards, and 206 is thrown both before a
// rotation and after a successful vset_changed.
func TestWhichRefusalsMeanTheRoundMovedPast(t *testing.T) {
	for code, want := range map[int]bool{
		202: true, 204: true, 207: true, exitTypeCheck: true,
		203: false, 205: false, 206: false, 0: false, 999: false, 106: false,
	} {
		if got := (Rejection{Code: code}).MovedPast(); got != want {
			t.Errorf("code %d: MovedPast is %v, want %v", code, got, want)
		}
	}
}

// TestAStaleReadIsReadAgainAtOnce is the case from poker1 on 2026-09-24: the confirming read still
// showed the round as held, the send was answered round_not_found, and the service waited a full
// minute to notice a transition that had already happened.
func TestAStaleReadIsReadAgainAtOnce(t *testing.T) {
	p, _ := newPoker(refusal(exitTypeCheck))
	poke := Poke{Op: OpFinishParticipation, RoundSince: prevRound}

	p.movedPast = false
	p.attempt(context.Background(), []Poke{poke}, quiet())
	if !p.movedPast {
		t.Fatal("a send answered round_not_found did not mark the read as behind the chain")
	}

	wait, reason, ok := p.rereadIfStale(false)
	if !ok || wait != BurstTick {
		t.Fatalf("the next read is in %v (%q, ok=%v); a finished round would look outstanding for a minute", wait, reason, ok)
	}
}

// Refusals that do not say the state went forwards must leave the ordinary schedule alone, or a
// poke that is merely early would re-read the chain every second on top of its burst.
func TestOtherRefusalsDoNotTriggerAnEarlyRead(t *testing.T) {
	for _, code := range []int{203, 205, 206, 999} {
		p, _ := newPoker(refusal(code))
		p.movedPast = false
		p.attempt(context.Background(), []Poke{{Op: OpVsetChanged, RoundSince: currRound}}, quiet())
		if p.movedPast {
			t.Fatalf("code %d marked the read as stale", code)
		}
		if _, _, ok := p.rereadIfStale(false); ok {
			t.Fatalf("code %d scheduled an early re-read", code)
		}
	}
}

// TestEarlyRereadsAreBounded. The failure this must not create: an endpoint whose reads are stuck
// on an old block while sends reach a live one answers "due" and "moved past" on every cycle, and
// an unbounded early re-read would turn that into a one-second loop that never ends.
func TestEarlyRereadsAreBounded(t *testing.T) {
	p, _ := newPoker()

	for i := 0; i < maxStaleRereads; i++ {
		p.movedPast = true
		if _, _, ok := p.rereadIfStale(false); !ok {
			t.Fatalf("early re-read %d of %d was refused", i+1, maxStaleRereads)
		}
	}
	for i := 0; i < 20; i++ {
		p.movedPast = true
		if _, _, ok := p.rereadIfStale(false); ok {
			t.Fatalf("still re-reading at the tick after %d stale reads in a row; this is a hot loop", maxStaleRereads+i+1)
		}
	}

	// One cycle that is not refused that way re-arms it, so the next genuine stale read is still
	// caught quickly.
	p.movedPast = false
	if _, _, ok := p.rereadIfStale(false); ok {
		t.Fatal("a clean cycle scheduled an early re-read")
	}
	p.movedPast = true
	if _, _, ok := p.rereadIfStale(false); !ok {
		t.Fatal("the bound never re-armed after a clean cycle")
	}
}

// In blind mode round_not_found is the ordinary answer to a guessed round. It says nothing about
// a read, because there was none, and two dozen of them a cycle must not speed the loop up.
func TestBlindModeNeverReadsEarly(t *testing.T) {
	p, _ := newPoker()
	p.movedPast = true
	if _, _, ok := p.rereadIfStale(true); ok {
		t.Fatal("blind mode re-read at the tick because a guessed round was not found")
	}
	if p.staleRereads != 0 {
		t.Fatalf("blind mode counted %d stale reads", p.staleRereads)
	}
}

// TestACycleReadsEarlyOnceAndThenSettlesDown walks the sequence from the day through the same two
// calls Cycle makes, because the pieces were each right before and the bug was in how they were
// joined: a confirming read that is behind the chain, then the read that has caught up.
func TestACycleReadsEarlyOnceAndThenSettlesDown(t *testing.T) {
	p, _ := newPoker(refusal(exitTypeCheck))
	stale := []Poke{{Op: OpFinishParticipation, RoundSince: prevRound}}

	// The read says the round is still held, so the poke is due; the treasury says it is gone.
	p.firstAttempt(context.Background(), stale, quiet())
	if wait, reason := p.nextWait(View{}, true, false); wait != BurstTick {
		t.Fatalf("after a moved-past refusal the next read is in %v (%v), not at the tick", wait, reason)
	}

	// The next read has caught up: nothing is due, nothing is sent. The mark from the cycle before
	// must not carry over, or every quiet cycle after one stale read would also read early.
	p.firstAttempt(context.Background(), nil, quiet())
	if wait, reason := p.nextWait(View{}, false, false); wait == BurstTick {
		t.Fatalf("a cycle with nothing due read early (%v): the stale mark outlived its cycle", reason)
	}
	if p.staleRereads != 0 {
		t.Fatalf("the bound was not re-armed by a clean cycle: %d", p.staleRereads)
	}
}

// A burst's own confirming read comes first and is not counted against the bound - the burst
// already explains why the next read is soon.
func TestABurstsConfirmingReadIsNotAStaleRead(t *testing.T) {
	p, _ := newPoker()
	p.movedPast = true
	wait, reason := p.nextWait(View{}, true, true)
	if wait != BurstTick || reason != "confirming the burst" {
		t.Fatalf("after a burst the wait is %v (%v)", wait, reason)
	}
	if p.staleRereads != 0 {
		t.Fatalf("a burst spent %d of the stale-read bound", p.staleRereads)
	}
}
