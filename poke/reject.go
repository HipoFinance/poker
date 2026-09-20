package poke

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/xssnick/tonutils-go/ton"
)

// A rejection is the treasury declining an external, and for this service it is the ordinary
// case rather than a fault.
//
// The whole design rests on it: every guard runs before accept_message, so a poke aimed a second
// early, or a second copy of one that already worked, is thrown away with no transaction and no
// cost to anyone. Two instances and a one-second burst are affordable precisely because of it.
//
// It arrives as a transport-looking error - the liteserver ran the message against the current
// state, the contract threw, and the node reports that it could not apply it - which is why this
// has to be told apart from an actual transport failure. Treating the two the same logged the
// normal case as a warning and counted it into hipo_poker_poke_errors_total, which is what
// PokerNotSending alerts on; it would have paged about every round.
type Rejection struct {
	// Code is the contract's exit code, or 0 if the node did not report one.
	Code int
}

// lsErrNotAccepted is the liteserver's code for "I ran this against the current state and the
// contract refused it".
const lsErrNotAccepted = -701

var exitCodePattern = regexp.MustCompile(`exitcode=(-?\d+)`)

// treasuryErrors names the exit codes an external can produce, from
// contracts/imports/constants.fc. A code not listed is reported as its number.
var treasuryErrors = map[int]string{
	106: "stopped",
	107: "invalid_op",
	201: "not_accepting_loan_requests",
	202: "unable_to_participate",
	203: "too_soon_to_participate",
	204: "not_ready_to_finish_participation",
	205: "too_soon_to_finish_participation",
	206: "vset_not_changed",
	207: "vset_not_changeable",
	208: "not_ready_to_burn_all",
	304: "unexpected_validator_set_format",
}

// Reason names the guard that refused the message.
func (r Rejection) Reason() string {
	if name, ok := treasuryErrors[r.Code]; ok {
		return fmt.Sprintf("%v (%d)", name, r.Code)
	}
	if r.Code == 0 {
		return "no exit code reported"
	}
	return fmt.Sprintf("exit code %d", r.Code)
}

// Expected reports whether this rejection is one the service causes in normal operation, as
// opposed to one that says something is wrong.
//
// The first three are a poke arriving a moment early - the guard compares against the including
// block's gen_utime, which can precede the send. The last three are a poke arriving after the
// work is already done, by an earlier copy of itself, by the other instance, or by a borrower.
// Nothing about either is worth waking anyone for.
func (r Rejection) Expected() bool {
	switch r.Code {
	case 202, 203, 204, 205, 206, 207:
		return true
	}
	return false
}

// asRejection reports whether an error is the treasury refusing the message rather than a failure
// to reach the chain at all. The distinction decides three things: whether it is logged as a
// warning, whether it counts as a send failure, and whether the poke counts as having left - a
// refused message did reach a node and was run, so it did.
func asRejection(err error) (Rejection, bool) {
	var ls ton.LSError
	if !errors.As(err, &ls) || ls.Code != lsErrNotAccepted {
		return Rejection{}, false
	}
	var code int
	if m := exitCodePattern.FindStringSubmatch(ls.Text); m != nil {
		code, _ = strconv.Atoi(m[1])
	}
	return Rejection{Code: code}, true
}
