package poke

import (
	"errors"
	"fmt"
	"testing"

	"github.com/xssnick/tonutils-go/ton"
)

// The three refusals mainnet actually produced on 2026-09-19/20, verbatim from the service's own
// logs. Every one of them is the contract working as designed, and every one was logged as a
// warning and counted as a send failure.
const (
	refusedFinish = "cannot apply external message to current state : External message was not accepted: " +
		"cannot run message on account: inbound external message rejected by transaction " +
		"8BC991CFE177BC7E9721433EFA3BEFD199485A55CFFD040A06C89AF026B71BCF:\nexitcode=204, steps=95, gas_used=0\n" +
		"VM Log (truncated):\n...cute LDDICT\nexecute ENDS\nexecute EQINT 4\nexecute THROWIFNOT 204\n" +
		"default exception handler, terminating vm with exit code 204"
	refusedParticipate = "cannot apply external message to current state : External message was not accepted: " +
		"cannot run message on account: inbound external message rejected by transaction X:\n" +
		"exitcode=203, steps=127, gas_used=0\nVM Log (truncated):\nexecute THROWIFNOT 203"
	refusedVset = "cannot apply external message to current state : External message was not accepted: " +
		"cannot run message on account: inbound external message rejected by transaction X:\n" +
		"exitcode=206, steps=109, gas_used=0\nVM Log (truncated):\nexecute THROWIFNOT 206"
)

func lsRefusal(text string) error { return ton.LSError{Code: lsErrNotAccepted, Text: text} }

// TestAsRejectionReadsTheRealRefusals. A refused external arrives looking like a transport error -
// the node says it could not apply the message - so the two have to be told apart by the code, or
// the ordinary case is reported as a fault.
func TestAsRejectionReadsTheRealRefusals(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		code   int
		reason string
	}{
		{"the burst racing its own success", lsRefusal(refusedFinish), 204, "not_ready_to_finish_participation (204)"},
		{"a poke one second early", lsRefusal(refusedParticipate), 203, "too_soon_to_participate (203)"},
		{"someone else already moved the round", lsRefusal(refusedVset), 206, "vset_not_changed (206)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, ok := asRejection(tt.err)
			if !ok {
				t.Fatal("a refusal by the treasury was read as a failure to reach the chain")
			}
			if r.Code != tt.code {
				t.Fatalf("exit code %d, want %d", r.Code, tt.code)
			}
			if r.Reason() != tt.reason {
				t.Fatalf("reason %q, want %q", r.Reason(), tt.reason)
			}
			if !r.Expected() {
				t.Fatalf("%v was treated as worth waking someone for", r.Reason())
			}
		})
	}
}

// A real transport failure must NOT be read as a refusal, or the one error that deserves a warning
// stops producing one and PokerNotSending goes blind.
func TestAsRejectionIgnoresTransportFailures(t *testing.T) {
	for _, err := range []error{
		errors.New("context deadline exceeded"),
		fmt.Errorf("failed to serialize external message, err: %w", errors.New("boom")),
		ton.LSError{Code: -400, Text: "not ready"},
		ton.LSError{Code: 651, Text: "block is not applied"},
	} {
		if r, ok := asRejection(err); ok {
			t.Fatalf("%v was read as a refusal with %v", err, r.Reason())
		}
	}
}

// A refusal with no readable exit code, or one the contract does not produce from an external,
// still has to be reported rather than swallowed - it is the case where something is genuinely
// wrong and the service does not know what.
func TestAsRejectionHandlesUnknownCodes(t *testing.T) {
	r, ok := asRejection(lsRefusal("External message was not accepted: something new"))
	if !ok {
		t.Fatal("a refusal without an exit code was not recognised as one")
	}
	if r.Expected() {
		t.Fatal("an unreadable refusal was treated as routine")
	}
	if r.Reason() != "no exit code reported" {
		t.Fatalf("reason %q", r.Reason())
	}

	// err::stopped is not something these three ops can produce - none of them checks stopped? -
	// so seeing it would mean the contract has changed under us.
	stopped, _ := asRejection(lsRefusal("exitcode=106, steps=1, gas_used=0"))
	if stopped.Expected() {
		t.Fatal("err::stopped from an external was treated as routine")
	}
	if stopped.Reason() != "stopped (106)" {
		t.Fatalf("reason %q", stopped.Reason())
	}
}

// The rest of what mainnet produced on 2026-09-20, once the service was made to poke blind. Both
// of these were logged as faults and neither is one.
const (
	refusedUnknownRound = "cannot apply external message to current state : External message was not accepted: " +
		"cannot run message on account: inbound external message rejected by transaction X:\n" +
		"exitcode=7, steps=33, gas_used=0\nVM Log (truncated):\nexecute LDU 32\nexecute LDDICT"
	duplicateSend = "cannot send external message : duplicate message"
)

// TestRoundNotFoundIsOrdinary. Exit code 7 is the treasury saying it has no such round: all three
// handlers udict_get the participation and hand the miss straight to unpack_participation, which
// loads from null.
//
// It used to warn outside blind mode, on the theory that a sighted cycle only pokes rounds it just
// read. Mainnet disagreed within a day: at 2026-09-22 00:43:56 a poke went out against a read two
// seconds old, for a round that had recovered and been deleted in between - because this service
// had just driven it there. The alternative reading, that participations no longer unpack as
// expected, cannot reach here: it fails parseTreasuryState first and that is blind mode.
func TestRoundNotFoundIsOrdinary(t *testing.T) {
	r, ok := asRejection(lsRefusal(refusedUnknownRound))
	if !ok {
		t.Fatal("exit code 7 was not read as a refusal")
	}
	if r.Code != exitTypeCheck {
		t.Fatalf("exit code %d, want %d", r.Code, exitTypeCheck)
	}
	if r.Reason() != "round_not_found (7)" {
		t.Fatalf("reason %q", r.Reason())
	}
	if !r.Expected() {
		t.Fatal("a round the treasury does not hold was reported as a fault")
	}
	if !r.Settled() {
		t.Fatal("the burst kept re-sending at a round the treasury does not hold")
	}
}

// TestDuplicateIsNotAFailure. Both instances build the same body - same op, same round, and a
// query id that is the chain clock in seconds - so one of them is routinely told the other's copy
// is already queued. The message is at a node, which is what sending means here.
func TestDuplicateIsNotAFailure(t *testing.T) {
	err := ton.LSError{Code: 0, Text: duplicateSend}
	if !isDuplicate(err) {
		t.Fatal("a queued duplicate was read as a failure to send")
	}
	if _, ok := asRejection(err); ok {
		t.Fatal("a duplicate was read as the treasury refusing the message")
	}

	// Everything else at code 0 is a real failure and must keep its warning.
	for _, other := range []error{
		ton.LSError{Code: 0, Text: "cannot send external message : too many requests"},
		errors.New("context deadline exceeded"),
	} {
		if isDuplicate(other) {
			t.Fatalf("%v was read as a duplicate", other)
		}
	}
}

// duplicateErr is the error a node returns when it already holds the message, as the service
// sees it.
func duplicateErr() error { return ton.LSError{Code: 0, Text: duplicateSend} }
