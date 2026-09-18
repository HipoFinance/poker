package poke

import (
	"math/big"
	"testing"

	"github.com/xssnick/tonutils-go/tvm/cell"
)

// participationCell builds a participation exactly as pack_participation in treasury.fc lays it
// out: state:uint4 size:uint16, seven dictionaries, total_staked and total_recovered as Coins,
// current_vset_hash:uint256, stake_held_for:uint32, stake_held_until:uint32.
func participationCell(t *testing.T, state State, vsetHash *big.Int, stakeHeldUntil uint32) *cell.Cell {
	t.Helper()
	b := cell.BeginCell().
		MustStoreUInt(uint64(state), 4).
		MustStoreUInt(0, 16)
	for i := 0; i < 7; i++ {
		b.MustStoreBoolBit(false) // an empty dict is a Maybe with no ref
	}
	return b.
		MustStoreBigCoins(big.NewInt(0)).
		MustStoreBigCoins(big.NewInt(0)).
		MustStoreBigUInt(vsetHash, 256).
		MustStoreUInt(65536, 32).
		MustStoreUInt(uint64(stakeHeldUntil), 32).
		EndCell()
}

// participationsDict is the dictionary the treasury keeps at get_treasury_state index 7.
func participationsDict(t *testing.T, rounds map[uint32]*cell.Cell) *cell.Cell {
	t.Helper()
	d := cell.NewDict(32)
	for round, c := range rounds {
		if err := d.SetIntKey(big.NewInt(int64(round)), c); err != nil {
			t.Fatalf("set round %d: %v", round, err)
		}
	}
	c, err := d.ToCell()
	if err != nil {
		t.Fatalf("participations dict: %v", err)
	}
	return c
}

// vsetCell builds a validator set as get_vset_times reads it:
//
//	validators_ext#12 utime_since:uint32 utime_until:uint32 total:uint16 main:uint16
//	  total_weight:uint64 list:(HashmapE 16 ValidatorDescr) = ValidatorSet;
func vsetCell(t *testing.T, since, until uint32) *cell.Cell {
	t.Helper()
	return cell.BeginCell().
		MustStoreUInt(0x12, 8).
		MustStoreUInt(uint64(since), 32).
		MustStoreUInt(uint64(until), 32).
		MustStoreUInt(1, 16).
		MustStoreUInt(1, 16).
		MustStoreUInt(1, 64).
		MustStoreBoolBit(false).
		EndCell()
}

// stateTuple builds a get_treasury_state answer of the right length with participations at index
// 7 and stopped? at index 9, and harmless integers everywhere else.
func stateTuple(participations any, stopped int64) []any {
	tuple := make([]any, treasuryStateMinFields)
	for i := range tuple {
		tuple[i] = big.NewInt(0)
	}
	tuple[treasuryParticipationsIndex] = participations
	tuple[treasuryStoppedIndex] = big.NewInt(stopped)
	return tuple
}

func timesTuple(currentRoundSince, participateSince, participateUntil, nextRoundSince, nextRoundUntil, stakeHeldFor uint32) []any {
	return []any{
		big.NewInt(int64(currentRoundSince)),
		big.NewInt(int64(participateSince)),
		big.NewInt(int64(participateUntil)),
		big.NewInt(int64(nextRoundSince)),
		big.NewInt(int64(nextRoundUntil)),
		big.NewInt(int64(stakeHeldFor)),
	}
}
