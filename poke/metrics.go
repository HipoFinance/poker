package poke

import (
	"strconv"
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

	PokeErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hipo_poker_poke_errors_total",
		Help: "Sends that no liteserver accepted, by op.",
	}, []string{"op"})

	// Confirmed counts observed state transitions, which is the only evidence a poke worked.
	Confirmed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hipo_poker_confirmed_transitions_total",
		Help: "State transitions observed after a poke, by op.",
	}, []string{"op"})

	// TreasuryStateFields is the observed get_treasury_state tuple length, so a shape change is
	// visible here even on the cycles where the guard still passed.
	TreasuryStateFields = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_treasury_state_fields",
		Help: "Observed length of the get_treasury_state tuple.",
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
	LastReadSuccess.Set(float64(time.Now().Unix()))
	TreasuryStateFieldsExpected.Set(treasuryStateMinFields)
	BlindModeSince.Set(0)
	ClockSynced.Set(0)
}

// published is the label set currently exported by UnconfirmedPoke, so that stale series can be
// removed one at a time instead of clearing the whole vector.
var published = map[[2]string]bool{}

// PublishOutstanding replaces the unconfirmed-poke series with the current set, so that a
// confirmed poke's series disappears instead of ageing forever and every alert built on it
// self-resolves.
//
// It deletes what is gone rather than calling Reset first. Reset-then-refill leaves a window in
// which the vector is empty, and a scrape landing inside it reads as "nothing outstanding" - which
// on an alert with no `for:` shows up as a resolve followed by a re-fire. The window is tiny and
// the scrape is every 15 seconds, so it would be rare, confusing and very hard to reproduce.
func PublishOutstanding(outstanding map[Poke]time.Duration) {
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
