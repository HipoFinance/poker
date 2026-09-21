package poke

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// fakeSender answers each send from a script and records when it was asked, so that the cadence
// can be asserted rather than described. The two-second cadence shipped because it was described.
type fakeSender struct {
	at      []time.Duration
	answers []error // consumed in order; the last one repeats
	started time.Time
}

func (f *fakeSender) Send(context.Context, *cell.Cell) error {
	f.at = append(f.at, time.Since(f.started))
	if len(f.answers) == 0 {
		return nil
	}
	answer := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	return answer
}

func newPoker(answers ...error) (*Poker, *fakeSender) {
	f := &fakeSender{answers: answers, started: time.Now()}
	p := &Poker{sender: f, clock: &Clock{}, tracker: NewTracker()}
	p.clock.Observe(testNow)
	return p, f
}

func refusal(code int) error {
	return ton.LSError{Code: lsErrNotAccepted, Text: "exitcode=" + itoa(code) + ", steps=1, gas_used=0"}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// TestBurstKeepsItsCadenceAndStops is the point of the whole change: attempts one second apart,
// not two, and a window that closes.
//
// 203 is "too soon" - the guard compared against a block's gen_utime that had not reached the
// deadline - which is exactly the case the burst exists for, so it never settles and the burst
// runs its full length.
func TestBurstKeepsItsCadenceAndStops(t *testing.T) {
	// Real time, because the cadence is the thing under test. Parallel so the window each one
	// waits out overlaps with the others rather than adding up.
	t.Parallel()

	p, f := newPoker(refusal(203))
	poke := Poke{Op: OpParticipateInElection, RoundSince: nextRound}

	// In a goroutine with a hard bound: a burst whose window never closes would otherwise hang
	// the suite until the whole test binary times out, which reads as a broken CI job rather
	// than as this assertion.
	start := time.Now()
	done := make(chan struct{})
	go func() { p.burst(context.Background(), []Poke{poke}); close(done) }()
	select {
	case <-done:
	case <-time.After(BurstTail + 5*BurstTick):
		t.Fatal("the burst never closed its window; it would send until the process stopped")
	}
	elapsed := time.Since(start)

	if elapsed < BurstTail || elapsed > BurstTail+2*BurstTick {
		t.Fatalf("the burst ran for %v, want about %v", elapsed, BurstTail)
	}

	want := int(BurstTail / BurstTick)
	if len(f.at) < want-1 || len(f.at) > want+1 {
		t.Fatalf("%d attempts in %v, want about %d - the cadence is not one per tick",
			len(f.at), BurstTail, want)
	}
	for i := 1; i < len(f.at); i++ {
		gap := f.at[i] - f.at[i-1]
		if gap < BurstTick/2 || gap > BurstTick*3/2 {
			t.Fatalf("attempt %d came %v after the one before, want about %v", i, gap, BurstTick)
		}
	}
}

// Each refusal code either keeps the poke in the burst or ends it, and getting 206 wrong in
// either direction is the expensive mistake: it is thrown both BEFORE a rotation and AFTER a
// successful vset_changed, because the handler packs new_vset_hash back into the participation.
func TestWhichRefusalsEndTheBurst(t *testing.T) {
	tests := []struct {
		name    string
		answer  error
		settled bool
	}{
		{"too soon to participate", refusal(203), false},
		{"too soon to finish", refusal(205), false},
		{"vset not changed, which may mean either thing", refusal(206), false},
		{"a duplicate is queued, not resolved", ton.LSError{Code: 0, Text: duplicateSend}, false},
		{"an accepted send proves nothing", nil, false},
		{"the round is no longer open", refusal(202), true},
		{"the round is no longer held", refusal(204), true},
		{"the round is neither staked nor validating", refusal(207), true},
		{"the treasury has no such round", refusal(7), true},
		{"a code this build does not know", refusal(999), true},
		{"nothing reached the chain", errors.New("context deadline exceeded"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := newPoker(tt.answer)
			poke := Poke{Op: OpVsetChanged, RoundSince: currRound}
			_, unsettled := p.attempt(context.Background(), []Poke{poke}, func(string, string) {})
			if tt.settled && len(unsettled) != 0 {
				t.Fatalf("the burst would keep re-sending after %v", tt.name)
			}
			if !tt.settled && len(unsettled) != 1 {
				t.Fatalf("the burst gave up after %v, costing a whole retry interval", tt.name)
			}
		})
	}
}

// A poke refused every second must not refresh its own window. The bound is from the burst's
// first send, so an endlessly refused poke still ends at BurstTail.
func TestAnEndlesslyRefusedPokeStillEndsItsBurst(t *testing.T) {
	// Real time, because the cadence is the thing under test. Parallel so the window each one
	// waits out overlaps with the others rather than adding up.
	t.Parallel()

	p, f := newPoker(refusal(206))
	start := time.Now()
	p.burst(context.Background(), []Poke{{Op: OpVsetChanged, RoundSince: currRound}})
	if elapsed := time.Since(start); elapsed > BurstTail+2*BurstTick {
		t.Fatalf("a poke refused on every attempt kept its burst alive for %v", elapsed)
	}
	if len(f.at) == 0 {
		t.Fatal("the burst sent nothing at all")
	}
}

// A settled poke drops out and takes the burst with it when it is the last one, so the common
// case - the work is done - costs one extra attempt rather than ten.
func TestTheBurstStopsWhenNothingIsLeft(t *testing.T) {
	// Real time, because the cadence is the thing under test. Parallel so the window each one
	// waits out overlaps with the others rather than adding up.
	t.Parallel()

	p, f := newPoker(refusal(204))
	start := time.Now()
	p.burst(context.Background(), []Poke{{Op: OpFinishParticipation, RoundSince: prevRound}})
	if elapsed := time.Since(start); elapsed > 3*BurstTick {
		t.Fatalf("the burst kept going for %v after the treasury said the work was done", elapsed)
	}
	if len(f.at) != 1 {
		t.Fatalf("%d attempts after a settled refusal, want 1", len(f.at))
	}
}

// Cancelling the context ends a burst in progress rather than holding the process open for the
// rest of the window.
func TestABurstStopsOnCancellation(t *testing.T) {
	// Real time, because the cadence is the thing under test. Parallel so the window each one
	// waits out overlaps with the others rather than adding up.
	t.Parallel()

	p, _ := newPoker(refusal(203))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(2 * BurstTick); cancel() }()

	start := time.Now()
	p.burst(ctx, []Poke{{Op: OpParticipateInElection, RoundSince: nextRound}})
	if elapsed := time.Since(start); elapsed > BurstTail-BurstTick {
		t.Fatalf("a cancelled burst ran for %v", elapsed)
	}
}

// TestAWedgedRoundDoesNotSuppressAFreshBurst is the property that used to live in
// TestScheduleUsesTheNewestSend, at its new site.
//
// The old bug was `schedule` handing NextWait the OLDEST outstanding age, so one round wedged for
// ten minutes put every cycle on the plain retry and the round whose deadline had just arrived
// got no burst. The mechanism is gone - schedule no longer consults the tracker - but the
// property has to survive the move: a poke that has never been sent opens a burst even when a
// wedged one is due in the same cycle.
func TestAWedgedRoundDoesNotSuppressAFreshBurst(t *testing.T) {
	p, _ := newPoker()
	wedged := Poke{Op: OpParticipateInElection, RoundSince: nextRound}
	fresh := Poke{Op: OpFinishParticipation, RoundSince: prevRound}

	p.tracker.Observe([]Poke{wedged}, []Poke{wedged}, time.Now().Add(-10*time.Minute))

	if !p.shouldBurstBefore(View{}, []Poke{wedged, fresh}) {
		t.Fatal("a round whose deadline just arrived got no burst, because another round is wedged")
	}
	if p.shouldBurstBefore(View{}, []Poke{wedged}) {
		t.Fatal("a round wedged for ten minutes re-opened a burst; it would do so every retry")
	}
}

// TestNothingBurstsWhenNothingCanBeSent. The old shape of this bug was a hot loop: an empty
// tracker reports an age of zero, which read as "sent this instant" and pinned the cadence at one
// second forever in exactly the two states where that is least affordable. The shape is different
// now - a burst is ten seconds of sends rather than a fast sleep - but the two states are the
// same, and neither may produce one.
func TestNothingBurstsWhenNothingCanBeSent(t *testing.T) {
	p, _ := newPoker()
	due := []Poke{{Op: OpFinishParticipation, RoundSince: prevRound}}

	p.dryRun = true
	if p.shouldBurstBefore(View{}, due) {
		t.Fatal("DRY_RUN opened a burst; it sends nothing, so it would never settle")
	}
	p.dryRun = false

	if p.shouldBurstBefore(View{Blind: true}, due) {
		t.Fatal("blind mode opened a burst over two dozen guessed candidate rounds")
	}

	// And with every send failing, nothing is ever known to the tracker, so shouldBurstBefore
	// stays true forever - which is why the cycle also requires that something actually left.
	failing, f := newPoker(errors.New("dial tcp: connection refused"))
	sent, _ := failing.attempt(context.Background(), due, func(string, string) {})
	if len(sent) != 0 {
		t.Fatal("a send that never reached the chain was counted as having left")
	}
	if len(f.at) != 1 {
		t.Fatalf("%d attempts, want 1", len(f.at))
	}
}

// A burst must not make a poke look younger than it is: ages are measured from the first send,
// and PokerPokeUnconfirmed is built on them.
func TestABurstDoesNotResetAPokeAge(t *testing.T) {
	tr := NewTracker()
	poke := Poke{Op: OpVsetChanged, RoundSince: currRound}
	first := time.Now().Add(-90 * time.Second)

	tr.Observe([]Poke{poke}, []Poke{poke}, first)
	for i := 0; i < 10; i++ {
		tr.Observe([]Poke{poke}, []Poke{poke}, first.Add(time.Duration(i)*time.Second))
	}

	age := tr.Outstanding(time.Now())[poke]
	if age < 80*time.Second {
		t.Fatalf("after ten re-sends the poke reports an age of %v; the burst reset its clock "+
			"and a wedged round would never reach the alert threshold", age)
	}
}
