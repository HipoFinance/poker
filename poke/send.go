package poke

import (
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// Body builds one external message body. The encoding is op:uint32, query_id:uint64,
// round_since:uint32, matching route_external_message in treasury.fc and the three senders in
// the contract repository's wrappers/Treasury.ts.
func Body(p Poke, queryID uint64) *cell.Cell {
	return cell.BeginCell().
		MustStoreUInt(uint64(p.Op), 32).
		MustStoreUInt(queryID, 64).
		MustStoreUInt(uint64(p.RoundSince), 32).
		EndCell()
}

// QueryID is the id to stamp on an attempt: the chain's current time.
//
// It deliberately varies per attempt rather than being derived from the poke. A deterministic id
// would be nicer to read in an explorer and would let duplicate sends collapse to one message
// hash - but nodes keep a short cache of external message hashes they have already processed, and
// the attempts this service most needs to land are the ones a second or two after an attempt that
// was correctly rejected for being early. A stable hash risks that retry being swallowed as a
// duplicate at exactly the deadline it exists to hit. Varying the id makes every attempt a
// distinct message, and duplicates cost nothing anyway.
func QueryID(chainNow uint32) uint64 { return uint64(chainNow) }
