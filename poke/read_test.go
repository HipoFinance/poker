package poke

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xssnick/tonutils-go/ton"
)

// The 2026-09-20 failure, verbatim in shape: our own node answered CurrentMasterchainInfo with a
// block its own shard client had not caught up to, so every read against that block came back
// "is not in db". The public pool in the same process was healthy the whole time.
var laggingNode = errors.New("get_treasury_state: lite server error, code 651: cannot load block " +
	"(0,8000000000000000,98350264) is not in db (possibly out of sync: " +
	"shard_client_seqno=93898063 ls_seqno=93898106)")

func endpoints(names ...string) []*Endpoint {
	out := make([]*Endpoint, 0, len(names))
	for _, n := range names {
		out = append(out, &Endpoint{Name: n})
	}
	return out
}

// TestAReadFallsBackToTheNextEndpoint is the fix for the incident. One lagging node must not
// decide the cycle while another endpoint can answer.
func TestAReadFallsBackToTheNextEndpoint(t *testing.T) {
	eps := endpoints("own", "public")
	var tried []string

	step := func(_ context.Context, ep *Endpoint) (reading, error) {
		tried = append(tried, ep.Name)
		if ep.Name == "own" {
			return reading{network: NetworkConfig{CurrentSince: 1}, err: laggingNode}, nil
		}
		return reading{network: NetworkConfig{CurrentSince: 2}}, nil
	}

	r, err := readAcross(context.Background(), eps, step)
	if err != nil {
		t.Fatalf("a healthy second endpoint still failed the cycle: %v", err)
	}
	if r.err != nil {
		t.Fatalf("the reading kept the lagging node's failure: %v", r.err)
	}
	if r.network.CurrentSince != 2 {
		t.Fatal("the answer did not come from the endpoint that could answer")
	}
	if len(tried) != 2 || tried[0] != "own" {
		t.Fatalf("endpoints were tried as %v; own nodes come first because they are closer", tried)
	}
}

// A shape error is not a reason to ask somebody else. Every endpoint returns the same tuple, and
// asking around would only delay the blind mode that failure exists to trigger.
func TestAShapeErrorStopsAtTheFirstEndpoint(t *testing.T) {
	eps := endpoints("own", "public")
	tried := 0

	step := func(_ context.Context, ep *Endpoint) (reading, error) {
		tried++
		return reading{err: ShapeError{errors.New("26 fields, want 28")}}, nil
	}

	r, err := readAcross(context.Background(), eps, step)
	if err != nil {
		t.Fatalf("a shape error was reported as an unreachable chain: %v", err)
	}
	if !IsShapeError(r.err) {
		t.Fatalf("the shape error did not survive the read: %v", r.err)
	}
	if tried != 1 {
		t.Fatalf("asked %d endpoints about a tuple they all return identically", tried)
	}
}

// When every endpoint fails the treasury read, the cycle still has to come back with the
// validator sets, because blind mode has nothing to aim at without them.
func TestATotalTreasuryFailureKeepsTheNetworkConfig(t *testing.T) {
	eps := endpoints("own", "public")

	step := func(_ context.Context, ep *Endpoint) (reading, error) {
		return reading{network: NetworkConfig{CurrentSince: 7}, err: laggingNode}, nil
	}

	r, err := readAcross(context.Background(), eps, step)
	if err != nil {
		t.Fatalf("a treasury read failure was reported as no endpoint at all: %v", err)
	}
	if r.err == nil {
		t.Fatal("a failed treasury read was reported as a successful one")
	}
	if r.network.CurrentSince != 7 {
		t.Fatal("blind mode was left without the validator sets it needs to compute candidates")
	}
}

// And when no endpoint gives anything, that is the "no endpoint" case: nothing is claimed, and
// PokerReadFailing is what eventually says so.
func TestNoUsableEndpointIsAnError(t *testing.T) {
	step := func(_ context.Context, ep *Endpoint) (reading, error) {
		return reading{}, errors.New("dial tcp: connection refused")
	}

	if _, err := readAcross(context.Background(), endpoints("own", "public"), step); err == nil {
		t.Fatal("a cycle with no usable endpoint reported success")
	}
}

// TestChooseFailurePrefersADuplicate. A duplicate means the message is queued at a node; a
// timeout arriving later on the channel must not overwrite that, or a delivered poke is counted
// as a failure to send and the one-second burst switches itself off.
func TestChooseFailurePrefersADuplicate(t *testing.T) {
	dup := duplicateErr()
	timeout := errors.New("context deadline exceeded")

	for _, tt := range []struct {
		name     string
		failures []error
		want     error
	}{
		{"duplicate first", []error{dup, timeout}, dup},
		{"duplicate last", []error{timeout, dup}, dup},
		{"no duplicate", []error{errors.New("first"), timeout}, timeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := chooseFailure(tt.failures); got != tt.want {
				t.Fatalf("chose %v, want %v", got, tt.want)
			}
		})
	}

	if chooseFailure(nil) != nil {
		t.Fatal("no failures at all produced one")
	}
}

// summarise has to be stable across cycles, or logView's on-change suppression never suppresses
// anything and blind mode logs a fresh block every minute.
func TestSummariseIsStable(t *testing.T) {
	counts := map[string]int{"round_not_found (7)": 21, "already queued": 3, "vset_not_changed (206)": 3}
	want := "round_not_found (7) ×21, already queued ×3, vset_not_changed (206) ×3"
	for i := 0; i < 20; i++ {
		if got := summarise(counts); got != want {
			t.Fatalf("summarise is %q, want %q", got, want)
		}
	}
}

// What mainnet actually sent on 2026-10-06 at 19:42, when both vset_changed pokes had already been
// accepted and the burst was re-sending: our own nodes refused at once, and the public pool hung
// until its timeout.
var (
	ownRefusal    = lsRefusal(refusedVset) // -701, exitcode=206
	publicTimeout = errors.New("context deadline exceeded")
	unlabelled    = ton.LSError{Code: 0, Text: refusedWithoutACode}
)

// TestARefusalOutranksATimeout is the bug. chooseFailure returned the last error to arrive unless
// one was a duplicate, and the last to arrive is by construction the slowest endpoint - so a
// timeout overwrote a real refusal, and an ordinary "already done" was logged as "Failed to send",
// counted as a transport error, and dropped from the burst. Fourteen and seventeen times in
// eleven days.
func TestARefusalOutranksATimeout(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failures []error
		want     error
	}{
		{"the refusal arrived first, as it did on the day", []error{ownRefusal, publicTimeout}, ownRefusal},
		{"the refusal arrived last", []error{publicTimeout, ownRefusal}, ownRefusal},
		{"a refusal with no exit code still outranks a timeout", []error{unlabelled, publicTimeout}, unlabelled},
		{"a labelled refusal outranks an unlabelled one", []error{unlabelled, ownRefusal}, ownRefusal},
		{"a duplicate still comes first", []error{ownRefusal, duplicateErr(), publicTimeout}, duplicateErr()},
		{"with no verdict at all it is a transport error", []error{errors.New("first"), publicTimeout}, publicTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := chooseFailure(tt.failures); got != tt.want {
				t.Fatalf("chose %v, want %v", got, tt.want)
			}
		})
	}

	// And what the caller then does with it: a refusal must be read as one, or the fix stops at
	// the function boundary.
	chosen := chooseFailure([]error{ownRefusal, publicTimeout})
	if r, ok := asRejection(chosen); !ok || r.Code != 206 {
		t.Fatalf("the chosen failure is not read as the 206 refusal it is: %v", chosen)
	}
}

// feed returns a results channel that delivers each answer after its delay, the way endpoints do.
func feed(answers map[time.Duration]error) (<-chan error, int) {
	ch := make(chan error, len(answers))
	for after, err := range answers {
		go func(after time.Duration, err error) {
			time.Sleep(after)
			ch <- err
		}(after, err)
	}
	return ch, len(answers)
}

// TestSendDoesNotWaitOutAHungEndpoint. The other half: with the refusal preferred but still
// waited for, every send in the burst took the hung endpoint's full five seconds, and the burst
// made 5 attempts in 13 seconds where it should have made about 20.
func TestSendDoesNotWaitOutAHungEndpoint(t *testing.T) {
	t.Parallel()
	const grace = 100 * time.Millisecond
	hang := 2 * time.Second // stands in for sendTimeout

	results, n := feed(map[time.Duration]error{
		10 * time.Millisecond: ownRefusal,
		hang:                  publicTimeout,
	})
	start := time.Now()
	got := collect(results, n, grace)
	elapsed := time.Since(start)

	if got != ownRefusal {
		t.Fatalf("reported %v, want the refusal that had already arrived", got)
	}
	if elapsed > hang/2 {
		t.Fatalf("waited %v for an endpoint that never answered; the burst stalls on every send", elapsed)
	}
	if elapsed < grace {
		t.Fatalf("returned after %v, before the %v grace: a node a block ahead gets no chance to accept", elapsed, grace)
	}
}

// The grace exists for this: a node that refuses may be a block behind one that would accept, so
// an acceptance arriving inside it still wins.
func TestAnAcceptanceInsideTheGraceStillWins(t *testing.T) {
	t.Parallel()
	results, n := feed(map[time.Duration]error{
		10 * time.Millisecond: ownRefusal,
		60 * time.Millisecond: nil,
	})
	if got := collect(results, n, 500*time.Millisecond); got != nil {
		t.Fatalf("a refusal from a lagging node hid an acceptance from another: %v", got)
	}
}

// With no verdict from any node, nothing may be reported early: the only honest answer is that
// nothing reached the chain, and that needs every endpoint to have failed.
func TestWithNoVerdictSendWaitsForEveryEndpoint(t *testing.T) {
	t.Parallel()
	slow := 300 * time.Millisecond
	results, n := feed(map[time.Duration]error{
		10 * time.Millisecond: errors.New("dial tcp: connection refused"),
		slow:                  publicTimeout,
	})
	start := time.Now()
	got := collect(results, n, 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed < slow {
		t.Fatalf("gave up after %v with one endpoint still unheard and no verdict from any", elapsed)
	}
	if _, ok := asRejection(got); ok || isDuplicate(got) {
		t.Fatalf("a transport failure was reported as a verdict: %v", got)
	}
	if got == nil {
		t.Fatal("a send no endpoint took was reported as sent")
	}
}

// And an acceptance still returns at once, without waiting for anybody.
func TestAnAcceptanceReturnsImmediately(t *testing.T) {
	t.Parallel()
	results, n := feed(map[time.Duration]error{
		10 * time.Millisecond: nil,
		2 * time.Second:       publicTimeout,
	})
	start := time.Now()
	if got := collect(results, n, time.Second); got != nil {
		t.Fatalf("an accepted send was reported as %v", got)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("an accepted send waited %v for a slower endpoint", elapsed)
	}
}
