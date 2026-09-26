package poke

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The exported series. Note what is deliberately absent: there is no per-round participation
// state here. gauge already exports hipo_treasury_participation_state and the round-lifecycle
// alerts are built on it, and a second publisher of the same fact would only create a way for the
// two to disagree. These series are about the poker, not about the pool.
var (
	// LastReadSuccess is the unix time of the last cycle that read everything it needed. It is
	// initialised at process start and only ever moves forward, so a persistently failing read
	// shows up as a number that keeps growing rather than as an absent series a rule would have
	// to infer from something else going missing. Same construction as gauge's
	// hipo_treasury_last_success_timestamp_seconds, and for the same reason.
	LastReadSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_last_read_success_seconds",
		Help: "Unix time of the last fully successful chain read.",
	})

	// BlindMode is 1 while the treasury read is not trusted.
	BlindMode = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_blind_mode",
		Help: "1 while the poker cannot read or trust the treasury state and is poking blind.",
	})

	// BlindModeSince is the unix time blind mode started, or 0. The two-hour participate window
	// is measured from it, and the alert on it is what says the window has closed.
	BlindModeSince = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_blind_mode_since_seconds",
		Help: "Unix time blind mode started, or 0 when the treasury read is trusted.",
	})

	// UnconfirmedPoke is the age of each outstanding poke: one that has been sent and whose
	// transition has not yet been observed. This, not the send count, is the series that says the
	// protocol is not moving - a send only proves a liteserver took the bytes.
	UnconfirmedPoke = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hipo_poker_unconfirmed_poke_seconds",
		Help: "Seconds since a poke was first sent without its state transition being observed.",
	}, []string{"op", "round_since"})

	PokesSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hipo_poker_pokes_sent_total",
		Help: "External messages handed to a liteserver, by op.",
	}, []string{"op"})

	// PokeErrors counts sends that never reached the chain - a transport failure, not the
	// treasury refusing the message. That distinction is what PokerNotSending depends on: a
	// refusal is the ordinary case for this service, and counting it here would have paged about
	// every round.
	PokeErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hipo_poker_poke_errors_total",
		Help: "Sends that never reached the chain, by op. Excludes messages the treasury refused.",
	}, []string{"op"})

	// PokesRejected counts messages the treasury ran and refused, by the contract's exit code.
	// Expected in normal operation - 203 and 205 mean a poke arrived a moment early, 202, 204,
	// 206 and 207 mean the work was already done by an earlier copy, the other instance or a
	// borrower - so this is diagnostic and nothing alerts on it. A refusal that persists is
	// caught by PokerPokeUnconfirmed through the poke's age instead.
	PokesRejected = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hipo_poker_pokes_rejected_total",
		Help: "Externals the treasury ran and refused, by op and contract exit code.",
	}, []string{"op", "code"})

	// PokesDuplicate counts sends every endpoint refused because it already holds the same
	// message. This is a success wearing a failure's clothes: the two instances build identical
	// bodies on purpose, so one can be told the other's copy is already queued.
	//
	// Expect it to sit at ZERO in normal operation, and do not read that as a fault. Send fans
	// out to every endpoint and returns on the first success, so a duplicate is only reported
	// when EVERY endpoint had already seen that exact message - which needs the other instance to
	// have reached all of them within the same second, since the query id is the chain clock in
	// seconds. The instances wake independently, so they rarely collide. What does raise it is
	// blind mode, where both fire the same two dozen candidates on the same one-minute cadence.
	// The first full day on mainnet (2026-09-20) recorded none outside the blind-mode incident.
	PokesDuplicate = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hipo_poker_pokes_duplicate_total",
		Help: "Sends a node already had queued, by op. These are delivered, not failed.",
	}, []string{"op"})

	// LastTreasuryRead is the unix time of the last trusted read of get_treasury_state, as
	// opposed to LastReadSuccess, which only says the chain was reachable. The two differ for
	// exactly the window BlindTransportGrace covers: the treasury read is failing, blind mode has
	// not started yet, and nothing else says so.
	LastTreasuryRead = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_last_treasury_read_seconds",
		Help: "Unix time of the last trusted get_treasury_state read.",
	})

	// Confirmed counts observed state transitions, which is the only evidence a poke worked.
	Confirmed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hipo_poker_confirmed_transitions_total",
		Help: "State transitions observed after a poke, by op.",
	}, []string{"op"})

	// TreasuryStateFields is the observed get_treasury_state tuple length, so a shape change is
	// visible here even on the cycles where the guard still passed. It is -1, never 0, until the
	// get-method has answered once: 0 reads as "the tuple is empty", i.e. as a contract change,
	// when in fact nothing has been read at all - and the runbook tells an operator to go and
	// diff the contract on exactly that signal.
	TreasuryStateFields = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_treasury_state_fields",
		Help: "Observed length of the get_treasury_state tuple; -1 before it has ever answered.",
	})

	// ReadBlockSeqno is the masterchain block every read in a cycle was pinned to.
	//
	// This is the only series that says whether the chain data the poker acts on is CURRENT. A
	// liteserver that answers promptly from a stale block passes every other check here: reads
	// succeed, the tuple parses, and its wall clock is right so the offset is zero. Meanwhile the
	// poker never sees the state move and reports every poke as unconfirmed forever. gauge has
	// TreasuryStateStale for this and the v4 endpoint has a block-timestamp probe; this service
	// had nothing.
	ReadBlockSeqno = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_read_block_seqno",
		Help: "Masterchain seqno the last cycle's reads were pinned to.",
	})

	// DryRun is 1 when the process computes pokes and sends none. Exported because that state is
	// otherwise indistinguishable from a healthy poker: reads succeed, nothing is outstanding,
	// every series is green, and the protocol has no driver at all.
	DryRun = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_dry_run",
		Help: "1 when the poker is computing pokes without sending them.",
	})

	TreasuryStateFieldsExpected = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_treasury_state_fields_expected",
		Help: "Minimum get_treasury_state tuple length this build was written against.",
	})

	// ClockOffset is the correction applied to the host clock, in seconds. The poker copes with
	// any value, but a number far from zero means one of the two clocks is wrong - and the host's
	// being wrong also skews every `time() - <timestamp>` alert below, which Prometheus evaluates
	// against its own clock.
	ClockOffset = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_clock_offset_seconds",
		Help: "Chain time minus host time, in seconds.",
	})

	// ClockSynced is 0 until the chain's clock has actually been read. Without it an offset of
	// zero is ambiguous: it reads identically for "perfectly synced" and "never observed, so
	// every deadline is being computed on raw host time". That second case is what makes a badly
	// skewed host fire everything, confirm nothing, and look fine.
	ClockSynced = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_clock_synced",
		Help: "1 once the chain's clock has been observed at least once.",
	})

	// LastClockSuccess is when the chain's clock was last actually read. ClockSynced only ever
	// goes 0 to 1, so without this a GetTime that starts failing after one success leaves the
	// poker applying a frozen offset indefinitely with every clock series looking healthy.
	LastClockSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_last_clock_success_seconds",
		Help: "Unix time the chain's clock was last read.",
	})

	// BlindEntries counts transitions INTO blind mode, which is what makes an intermittently
	// failing read visible. A read that fails every few minutes never gives PokerBlindMode five
	// continuous minutes to fire, so without this it raises nothing while being just as broken as
	// a read that fails outright.
	BlindEntries = promauto.NewCounter(prometheus.CounterOpts{
		Name: "hipo_poker_blind_entries_total",
		Help: "Number of times the poker has entered blind mode.",
	})
)

func init() {
	precreateCounters()
	LastReadSuccess.Set(float64(time.Now().Unix()))
	LastTreasuryRead.Set(float64(time.Now().Unix()))
	TreasuryStateFieldsExpected.Set(treasuryStateMinFields)
	BlindModeSince.Set(0)
	ClockSynced.Set(0)
	LastClockSuccess.Set(float64(time.Now().Unix()))
	TreasuryStateFields.Set(-1)
	ReadBlockSeqno.Set(0)
	DryRun.Set(0)
}

// precreateCounters exports every counter series this service can produce, at zero, from the
// moment the process starts.
//
// A counter label set otherwise comes into existence on its first increment, and Prometheus never
// sees the increment that created it: rate() and increase() need a sample before the change, and
// there is none. For a counter born mid-burst it is worse - everything before the first scrape is
// lost. Measured on 2026-09-23: both code="206" series first appeared already at 5 and 10, which
// was the whole of the first rotation burst, and increase() over the next 48 hours reported 33
// refusals where the logs showed 48.
//
// The one that mattered is PokeErrors, because PokerNotSending is a rate over it. With no series
// until the first failure, the first transport failure of each op after every restart was
// invisible to the alert - which also happened to be hiding that the alert itself fired on a
// single blip. Both are fixed together; see operation's poker-alerting-rule.yaml.
//
// PokesRejected is created for every exit code this service knows how to name, plus 0 for a
// refusal reported without one. A code outside that set still gets a series on first sight, and
// losing that first increment is acceptable for a code nobody expected.
func precreateCounters() {
	precreate(AllOps, knownCodes(), []*prometheus.CounterVec{PokesSent, PokeErrors, PokesDuplicate, Confirmed}, PokesRejected)
}

// knownCodes is every exit code this service can name, plus 0 for a refusal reported without one.
func knownCodes() []string {
	codes := []string{"0"}
	for code := range treasuryErrors {
		codes = append(codes, strconv.Itoa(code))
	}
	return codes
}

// precreate is precreateCounters over vectors it is handed, so that it can be tested against fresh
// ones rather than the package's, which every other test in the binary is also incrementing.
func precreate(ops []Op, codes []string, perOp []*prometheus.CounterVec, rejected *prometheus.CounterVec) {
	for _, op := range ops {
		name := op.String()
		for _, vec := range perOp {
			vec.WithLabelValues(name)
		}
		for _, code := range codes {
			rejected.WithLabelValues(name, code)
		}
	}
}

// published is the label set currently exported by UnconfirmedPoke, so that stale series can be
// removed one at a time instead of clearing the whole vector.
//
// Package-level while the state it mirrors is per-Poker, and guarded because of it: one Poker per
// process is the only supported shape, but two in one process would otherwise silently delete
// each other's series with nothing erroring anywhere.
var (
	publishedMu sync.Mutex
	published   = map[[2]string]bool{}
)

// PublishOutstanding replaces the unconfirmed-poke series with the current set, so that a
// confirmed poke's series disappears instead of ageing forever and every alert built on it
// self-resolves.
//
// It deletes what is gone rather than calling Reset first. Reset-then-refill leaves a window in
// which the vector is empty, and a scrape landing inside it reads as "nothing outstanding" - which
// on an alert with no `for:` shows up as a resolve followed by a re-fire. The window is tiny and
// the scrape is every 15 seconds, so it would be rare, confusing and very hard to reproduce.
func PublishOutstanding(outstanding map[Poke]time.Duration) {
	publishedMu.Lock()
	defer publishedMu.Unlock()

	current := make(map[[2]string]bool, len(outstanding))
	for p, age := range outstanding {
		labels := [2]string{p.Op.String(), strconv.FormatUint(uint64(p.RoundSince), 10)}
		current[labels] = true
		UnconfirmedPoke.WithLabelValues(labels[0], labels[1]).Set(age.Seconds())
	}
	for labels := range published {
		if !current[labels] {
			UnconfirmedPoke.DeleteLabelValues(labels[0], labels[1])
		}
	}
	published = current
}
