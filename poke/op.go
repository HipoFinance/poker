package poke

import "fmt"

// Op is one of the three external message op-codes the treasury accepts. The values are
// op::participate_in_election, op::vset_changed and op::finish_participation in
// contracts/imports/constants.fc; route_external_message in treasury.fc accepts these three
// and throws err::invalid_op on anything else.
type Op uint32

const (
	OpParticipateInElection Op = 0x574a297b
	OpVsetChanged           Op = 0x2f0b5b3b
	OpFinishParticipation   Op = 0x23274435
)

// AllOps is the order pokes are sent in when nothing distinguishes them, which is blind mode.
// It is the order of the state machine itself, so a round that needs two transitions in a row
// gets them in the right sequence within a single cycle.
var AllOps = []Op{OpParticipateInElection, OpVsetChanged, OpFinishParticipation}

func (o Op) String() string {
	switch o {
	case OpParticipateInElection:
		return "participate_in_election"
	case OpVsetChanged:
		return "vset_changed"
	case OpFinishParticipation:
		return "finish_participation"
	}
	return fmt.Sprintf("unknown(0x%08x)", uint32(o))
}

// Poke is one external message: an op aimed at one round.
type Poke struct {
	Op         Op
	RoundSince uint32
}

func (p Poke) String() string {
	return fmt.Sprintf("%v(%v)", p.Op, p.RoundSince)
}
