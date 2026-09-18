package poke

import (
	"math/big"
	"strings"
	"testing"

	"github.com/xssnick/tonutils-go/tvm/cell"
)

// TestParseTreasuryStateRejectsWrongShapes is the test that would have caught 2026-09-06, when
// three fields were inserted into get_treasury_state, every index from 5 on moved, and the
// borrower tool - then the only off-chain sender of finish_participation - read a cell where it
// expected a number and cost the protocol a round.
//
// The contract of this function is not "parse the tuple". It is: anything that does not match the
// shape this build was written against must be reported as a failure, never interpreted. A
// failure becomes blind mode, which parses nothing and keeps working.
func TestParseTreasuryStateRejectsWrongShapes(t *testing.T) {
	good := participationsDict(t, map[uint32]*cell.Cell{
		currRound: participationCell(t, StateValidating, currentHash, 0),
	})

	tests := []struct {
		name  string
		tuple []any
		want  string
	}{
		{
			name:  "a tuple shorter than the shape we were written against",
			tuple: stateTuple(good, 0)[:treasuryStateMinFields-1],
			want:  "fewer than",
		},
		{
			name: "participations is not a cell",
			// What an insert looks like from here: a number lands where the dictionary was.
			tuple: stateTuple(big.NewInt(42), 0),
			want:  "not a cell",
		},
		{
			name:  "stopped? is not an integer",
			tuple: replace(stateTuple(good, 0), treasuryStoppedIndex, good),
			want:  "not an integer",
		},
		{
			name:  "stopped? is not a boolean",
			tuple: stateTuple(good, 7),
			want:  "not a boolean",
		},
		{
			name: "participations holds something that is not a participation",
			tuple: stateTuple(participationsDict(t, map[uint32]*cell.Cell{
				currRound: cell.BeginCell().MustStoreUInt(0, 64).EndCell(),
			}), 0),
			want: "participation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseTreasuryState(tt.tuple)
			if err == nil {
				t.Fatal("a wrong-shaped tuple was interpreted instead of rejected")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// A longer tuple is expected rather than suspicious: the getter is append-only, so a future
// release adding a field must not push this service into blind mode for no reason.
func TestParseTreasuryStateAcceptsAppendedFields(t *testing.T) {
	tuple := append(stateTuple(participationsDict(t, map[uint32]*cell.Cell{
		currRound: participationCell(t, StateValidating, currentHash, 0),
	}), 0), big.NewInt(1), big.NewInt(2))

	ts, err := parseTreasuryState(tuple)
	if err != nil {
		t.Fatalf("an appended field was treated as a shape change: %v", err)
	}
	if ts.Fields != treasuryStateMinFields+2 {
		t.Fatalf("observed %d fields, want %d", ts.Fields, treasuryStateMinFields+2)
	}
	if got := ts.Participations[currRound].State; got != StateValidating {
		t.Fatalf("round state %v, want validating", got)
	}
}

// An empty participations dictionary is a pool with no rounds in flight, not an error.
func TestParseTreasuryStateAcceptsNoParticipations(t *testing.T) {
	ts, err := parseTreasuryState(stateTuple(nil, 0))
	if err != nil {
		t.Fatalf("an empty participations dictionary was rejected: %v", err)
	}
	if len(ts.Participations) != 0 {
		t.Fatalf("got %d participations, want none", len(ts.Participations))
	}
}

func TestParseTreasuryStateReadsStopped(t *testing.T) {
	for _, tt := range []struct {
		raw  int64
		want bool
	}{{0, false}, {-1, true}, {1, true}} {
		ts, err := parseTreasuryState(stateTuple(nil, tt.raw))
		if err != nil {
			t.Fatalf("stopped? = %d: %v", tt.raw, err)
		}
		if ts.Stopped != tt.want {
			t.Fatalf("stopped? = %d read as %v, want %v", tt.raw, ts.Stopped, tt.want)
		}
	}
}

// ParseParticipation insists the cell is consumed exactly. A field read at the wrong width still
// decodes and still yields a plausible number; the leftover bits are the only thing that says the
// layout has drifted from the contract.
func TestParseParticipationInsistsOnExactConsumption(t *testing.T) {
	short := cell.BeginCell().
		MustStoreUInt(uint64(StateHeld), 4).
		MustStoreUInt(0, 16)
	for i := 0; i < 7; i++ {
		short.MustStoreBoolBit(false)
	}
	short.MustStoreBigCoins(big.NewInt(0)).
		MustStoreBigCoins(big.NewInt(0)).
		MustStoreBigUInt(currentHash, 256).
		MustStoreUInt(65536, 32)
	// stake_held_until missing entirely.
	if _, err := ParseParticipation(short.EndCell()); err == nil {
		t.Fatal("a participation missing its last field was accepted")
	}

	// The same layout with eight bits appended, which is what reading one field too narrow
	// leaves behind.
	long := cell.BeginCell().
		MustStoreUInt(uint64(StateHeld), 4).
		MustStoreUInt(0, 16)
	for i := 0; i < 7; i++ {
		long.MustStoreBoolBit(false)
	}
	longCell := long.
		MustStoreBigCoins(big.NewInt(0)).
		MustStoreBigCoins(big.NewInt(0)).
		MustStoreBigUInt(currentHash, 256).
		MustStoreUInt(65536, 32).
		MustStoreUInt(12, 32).
		MustStoreUInt(0, 8).
		EndCell()
	if _, err := ParseParticipation(longCell); err == nil {
		t.Fatal("a participation with trailing bits was accepted")
	} else if !strings.Contains(err.Error(), "bits left unread") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := ParseParticipation(nil); err == nil {
		t.Fatal("a nil cell was accepted")
	}
}

func TestParseTimesPinsItsLength(t *testing.T) {
	if _, err := parseTimes(timesTuple(1, 2, 3, 4, 5, 6)[:5]); err == nil {
		t.Fatal("a short get_times answer was accepted")
	}
	if _, err := parseTimes(append(timesTuple(1, 2, 3, 4, 5, 6), big.NewInt(7))); err == nil {
		t.Fatal("a long get_times answer was accepted")
	}
	times, err := parseTimes(timesTuple(currRound, electionAt, electionAt+600, nextRound, nextRound+65536, 32768))
	if err != nil {
		t.Fatalf("a correct get_times answer was rejected: %v", err)
	}
	if times.ParticipateSince != electionAt || times.StakeHeldFor != 32768 {
		t.Fatalf("misread get_times: %+v", times)
	}
}

func replace(tuple []any, index int, with any) []any {
	out := make([]any, len(tuple))
	copy(out, tuple)
	out[index] = with
	return out
}
