package poke

import (
	"fmt"
	"math/big"

	"github.com/xssnick/tonutils-go/tvm/cell"
)

// State is participation::* in contracts/imports/constants.fc.
type State uint8

const (
	StateOpen State = iota
	StateDistributing
	StateStaked
	StateValidating
	StateHeld
	StateRecovering
	StateReadyToBurn
	StateBurning
)

func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateDistributing:
		return "distributing"
	case StateStaked:
		return "staked"
	case StateValidating:
		return "validating"
	case StateHeld:
		return "held"
	case StateRecovering:
		return "recovering"
	case StateReadyToBurn:
		return "ready_to_burn"
	case StateBurning:
		return "burning"
	}
	return fmt.Sprintf("unknown(%d)", uint8(s))
}

// Participation is one round as the treasury stores it. Only State, CurrentVsetHash and
// StakeHeldUntil decide anything here; the rest is parsed because it has to be walked to reach
// them, and because walking all of it is what makes the consumption check below meaningful.
type Participation struct {
	State           State
	Size            uint16
	TotalStaked     *big.Int
	TotalRecovered  *big.Int
	CurrentVsetHash *big.Int
	StakeHeldFor    uint32
	StakeHeldUntil  uint32
}

// ParseParticipation reads one participation cell, following pack_participation in treasury.fc.
//
// It insists the cell is consumed exactly. That check is the point of this function rather than
// a tidiness flourish: this service finds the participations dictionary at a positional index of
// get_treasury_state, and if that index ever stops meaning "participations" - which it has done
// before, see the getter's own comment - some other dictionary lands here. Nearly any other cell
// fails to end precisely after 4 + 16 bits, seven dictionaries, two Coins, 256 bits and two
// uint32s, so this turns a silent misread into a read failure, and a read failure into blind mode.
func ParseParticipation(c *cell.Cell) (Participation, error) {
	var p Participation
	if c == nil {
		return p, fmt.Errorf("nil participation cell")
	}
	s := c.BeginParse()

	state, err := s.LoadUInt(4)
	if err != nil {
		return p, fmt.Errorf("state: %w", err)
	}
	if state > uint64(StateBurning) {
		return p, fmt.Errorf("state %d is not a participation state", state)
	}
	size, err := s.LoadUInt(16)
	if err != nil {
		return p, fmt.Errorf("size: %w", err)
	}
	// Key sizes as the contract uses them: sorted is keyed by the 120-bit sort key
	// (decide_loan_requests reads it with udict_get_max?(120)), the other six by the borrower's
	// 256-bit address. They do not affect this parse - store_dict is a Maybe ^Cell either way -
	// but a wrong one here would quietly produce a dictionary nobody could iterate.
	for i, keySz := range []uint{120, 256, 256, 256, 256, 256, 256} {
		if _, err := s.LoadDict(keySz); err != nil {
			return p, fmt.Errorf("dict %d: %w", i, err)
		}
	}
	totalStaked, err := s.LoadBigCoins()
	if err != nil {
		return p, fmt.Errorf("total_staked: %w", err)
	}
	totalRecovered, err := s.LoadBigCoins()
	if err != nil {
		return p, fmt.Errorf("total_recovered: %w", err)
	}
	vsetHash, err := s.LoadBigUInt(256)
	if err != nil {
		return p, fmt.Errorf("current_vset_hash: %w", err)
	}
	stakeHeldFor, err := s.LoadUInt(32)
	if err != nil {
		return p, fmt.Errorf("stake_held_for: %w", err)
	}
	stakeHeldUntil, err := s.LoadUInt(32)
	if err != nil {
		return p, fmt.Errorf("stake_held_until: %w", err)
	}
	if left := s.BitsLeft(); left != 0 {
		return p, fmt.Errorf("%d bits left unread: this is not a participation", left)
	}

	return Participation{
		State:           State(state),
		Size:            uint16(size),
		TotalStaked:     totalStaked,
		TotalRecovered:  totalRecovered,
		CurrentVsetHash: vsetHash,
		StakeHeldFor:    uint32(stakeHeldFor),
		StakeHeldUntil:  uint32(stakeHeldUntil),
	}, nil
}
