package poke

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

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

// exitTypeCheck is TVM's type check error, and for these three handlers it has exactly one
// cause: the round is not in the treasury's dictionary.
//
// All three start with `participations.udict_get?(32, round_since)` and hand the result straight
// to unpack_participation. A miss returns null, unpack_participation loads from it, and the VM
// throws 7 - before accept_message, like every other refusal here. So 7 means "no such round",
// and blind mode produces it by design: it aims at every round_since the validator sets suggest,
// most of which the treasury has never held.
const exitTypeCheck = 7

var exitCodePattern = regexp.MustCompile(`exitcode=(-?\d+)`)

// treasuryErrors names the exit codes an external can produce, from
// contracts/imports/constants.fc. A code not listed is reported as its number.
var treasuryErrors = map[int]string{
	exitTypeCheck: "round_not_found",

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
//
// round_not_found depends on where the poke came from, which is why blind is a parameter rather
// than a property of the code. Blind mode guesses round numbers from the validator sets and most
// of its guesses are wrong by construction, so 7 is its ordinary answer. A sighted cycle pokes
// only rounds it just read out of the treasury's own dictionary, so 7 there means the round left
// between the read and the send, or that participations no longer unpack the way this service
// believes - and the second of those is the failure this whole service was built around.
func (r Rejection) Expected(blind bool) bool {
	switch r.Code {
	case 202, 203, 204, 205, 206, 207:
		return true
	case exitTypeCheck:
		return blind
	}
	return false
}

// Settled reports whether this refusal ends the burst for that poke - that is, whether the
// treasury has said something that means there is no point sending it again without reading.
//
// It is a different question from Expected, over the same codes, and they disagree. Expected asks
// whether to warn a human; Settled asks whether to stop retrying. 206 is expected and NOT settled;
// an unreadable code is unexpected and IS settled.
//
// Only three codes keep a poke in the burst, and the logic is inverted deliberately so that an
// unrecognised code stops rather than loops:
//
//   - 203 too_soon_to_participate and 205 too_soon_to_finish_participation are a guard comparing
//     against a block's gen_utime that has not reached the deadline yet. Another second may be
//     all it needs. This is the case the burst exists for.
//   - 206 vset_not_changed is ambiguous and cannot be resolved without a read. vset_changed throws
//     it on `new_vset_hash != current_vset_hash` failing, which happens BEFORE the rotation, when
//     the stored hash still matches the config - and equally AFTER a successful vset_changed,
//     because the handler packs new_vset_hash back into the participation. Same code, opposite
//     meanings. So the burst keeps going: retrying a round that is already done costs a discarded
//     external, and dropping one that is not costs a minute.
//
// Everything else settles. 202, 204 and 207 are a state that has moved past this op, 7 is a round
// the treasury does not hold, and a code this service does not recognise is a reason to stop and
// let the next full cycle look at the chain rather than to keep firing blind.
func (r Rejection) Settled() bool {
	switch r.Code {
	case 203, 205, 206:
		return false
	}
	return true
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

// duplicateText is what a node answers when it already holds this exact external message.
//
// It arrives as an LSError with code 0 and no exit code, which is indistinguishable from a real
// transport failure by shape alone, so it is matched on the text. Nodes key their queue on the
// message hash and the two instances build identical bodies - same op, same round, and a query id
// that is the chain's clock in seconds - so whichever sends second is told the first one's copy
// is already there.
//
// That is a success, not a failure: the poke is at a node and will be run. It is also free
// deduplication, which is why the query id is deliberately left colliding rather than salted per
// instance. Two instances then cost the network one message instead of two.
const duplicateText = "duplicate message"

// isDuplicate reports whether the error is a node saying it already has this message queued.
func isDuplicate(err error) bool {
	var ls ton.LSError
	if !errors.As(err, &ls) {
		return false
	}
	return strings.Contains(ls.Text, duplicateText)
}
