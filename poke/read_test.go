package poke

import (
	"context"
	"errors"
	"testing"
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
