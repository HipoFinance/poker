package poke

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/xssnick/tonutils-go/tvm/cell"
)

// Where this service reads get_treasury_state, and the shape it was written against.
//
// The tuple is append-only by the rule written above the getter in treasury.fc, so these indices
// are stable going forward. They have not always been: on 2026-09-06 three fields were INSERTED
// and every index from 5 on moved, which broke the borrower tool - at the time the only off-chain
// sender of finish_participation - and cost a round. This service exists to be the thing that
// still works when that happens, so it must never inherit that failure.
//
// Hence the guard in ReadTreasury: the tuple's length, the type at each index this service
// touches, and the exact consumption of every participation cell are all checked before anything
// is believed. A tuple that does not match is not interpreted at all; it is a read failure, and a
// read failure is blind mode, which parses nothing.
const (
	treasuryParticipationsIndex = 7
	treasuryStoppedIndex        = 9

	// The shape as of the 2026-09-08 release (26 fields, mid_rate and mid_round appended).
	// Append-only means a longer tuple is expected and fine; a shorter one is not.
	treasuryStateMinFields = 26

	// get_times returns exactly six values and is not an append-only interface, so it is pinned
	// exactly rather than as a floor.
	treasuryTimesFields = 6
)

// Times is get_times in treasury.fc.
type Times struct {
	CurrentRoundSince uint32
	ParticipateSince  uint32
	ParticipateUntil  uint32
	NextRoundSince    uint32
	NextRoundUntil    uint32
	StakeHeldFor      uint32
}

// TreasuryState is everything the treasury itself tells this service.
type TreasuryState struct {
	Participations map[uint32]Participation
	Stopped        bool
	Times          Times
	// Fields is the observed length of the get_treasury_state tuple, exported so that a shape
	// change is visible in the metrics even on the cycles where it did not break anything.
	Fields int
}

// Rounds returns the round_since keys, oldest first.
func (t TreasuryState) Rounds() []uint32 {
	out := make([]uint32, 0, len(t.Participations))
	for r := range t.Participations {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ShapeError is a treasury state that came back and was not believed: the tuple's length, the
// type at an index this service reads, or a participation cell did not match what it was written
// against.
//
// It is kept apart from a failure to reach the chain because the two want opposite responses. A
// shape error is the 2026-09-06 incident recurring - it reads identically from every endpoint and
// on every retry, so there is nothing to wait for and blind mode has to start at once. A
// liteserver that cannot answer is usually well again within a minute, and going blind for that
// fires two dozen externals, takes the halt guard off for two hours and tells nobody the
// difference.
type ShapeError struct{ Err error }

func (e ShapeError) Error() string { return e.Err.Error() }
func (e ShapeError) Unwrap() error { return e.Err }

// IsShapeError reports whether a failed read was the state's shape rather than the chain's
// availability.
func IsShapeError(err error) bool {
	var shape ShapeError
	return errors.As(err, &shape)
}

// ReadTreasury reads and validates the treasury's own view. It never returns a partially trusted
// state: either the whole read is believed, or it is an error, and ShapeError distinguishes the
// kind of error it is.
func (s *Session) ReadTreasury(ctx context.Context) (TreasuryState, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	res, err := s.Endpoint.api.RunGetMethod(ctx, s.Block, s.Treasury, "get_treasury_state")
	if err != nil {
		return TreasuryState{}, fmt.Errorf("get_treasury_state: %w", err)
	}
	ts, err := parseTreasuryState(res.AsTuple())
	if err != nil {
		return ts, ShapeError{err}
	}

	timesRes, err := s.Endpoint.api.RunGetMethod(ctx, s.Block, s.Treasury, "get_times")
	if err != nil {
		return TreasuryState{}, fmt.Errorf("get_times: %w", err)
	}
	ts.Times, err = parseTimes(timesRes.AsTuple())
	if err != nil {
		return TreasuryState{}, ShapeError{err}
	}

	return ts, nil
}

// parseTreasuryState is the guard, kept separate from the transport so that it can be tested
// against a tuple of the wrong shape without a chain. Every check here exists because the
// alternative is believing a number that means something else.
func parseTreasuryState(tuple []any) (TreasuryState, error) {
	ts := TreasuryState{Fields: len(tuple), Participations: map[uint32]Participation{}}

	if len(tuple) < treasuryStateMinFields {
		return ts, fmt.Errorf("get_treasury_state returned %d fields, fewer than the %d this was written against",
			len(tuple), treasuryStateMinFields)
	}

	stopped, ok := tuple[treasuryStoppedIndex].(*big.Int)
	if !ok {
		return ts, fmt.Errorf("index %d (stopped?) is %T, not an integer", treasuryStoppedIndex, tuple[treasuryStoppedIndex])
	}
	// FunC true is -1; 1 is accepted as well rather than depending on which the getter yields.
	if !stopped.IsInt64() || (stopped.Int64() != 0 && stopped.Int64() != -1 && stopped.Int64() != 1) {
		return ts, fmt.Errorf("index %d (stopped?) is %v, which is not a boolean", treasuryStoppedIndex, stopped)
	}
	ts.Stopped = stopped.Sign() != 0

	switch participations := tuple[treasuryParticipationsIndex].(type) {
	case nil:
		// An empty participations dictionary. Normal for a pool with no rounds in flight.
	case *cell.Cell:
		dict, err := participations.BeginParse().ToDict(32)
		if err != nil {
			return ts, fmt.Errorf("index %d (participations) is not a dictionary keyed by uint32: %w",
				treasuryParticipationsIndex, err)
		}
		if err := loadParticipations(dict, ts.Participations); err != nil {
			return ts, err
		}
	default:
		return ts, fmt.Errorf("index %d (participations) is %T, not a cell",
			treasuryParticipationsIndex, tuple[treasuryParticipationsIndex])
	}

	return ts, nil
}

func loadParticipations(dict *cell.Dictionary, into map[uint32]Participation) error {
	for _, kv := range dict.All() {
		if kv.Key == nil || kv.Value == nil {
			return fmt.Errorf("participations holds an empty entry")
		}
		key, err := kv.Key.BeginParse().LoadUInt(32)
		if err != nil {
			return fmt.Errorf("participation key: %w", err)
		}
		p, err := ParseParticipation(kv.Value)
		if err != nil {
			return fmt.Errorf("participation %d: %w", key, err)
		}
		into[uint32(key)] = p
	}
	return nil
}

// parseTimes reads get_times, which returns exactly six values and is not an append-only
// interface, so its length is pinned exactly rather than as a floor.
func parseTimes(tuple []any) (Times, error) {
	var t Times
	if len(tuple) != treasuryTimesFields {
		return t, fmt.Errorf("get_times returned %d values, not %d", len(tuple), treasuryTimesFields)
	}
	out := make([]uint32, treasuryTimesFields)
	for i := range out {
		v, ok := tuple[i].(*big.Int)
		if !ok {
			return t, fmt.Errorf("get_times index %d is %T, not an integer", i, tuple[i])
		}
		if v.Sign() < 0 || !v.IsInt64() || v.Int64() > int64(^uint32(0)) {
			return t, fmt.Errorf("get_times index %d is %v, which is not a unix time", i, v)
		}
		out[i] = uint32(v.Int64())
	}
	return Times{
		CurrentRoundSince: out[0],
		ParticipateSince:  out[1],
		ParticipateUntil:  out[2],
		NextRoundSince:    out[3],
		NextRoundUntil:    out[4],
		StakeHeldFor:      out[5],
	}, nil
}
