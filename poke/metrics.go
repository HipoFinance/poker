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

	// ClockOffset is the correction applied to the host clock, in seconds. A number drifting away
	// from zero is a host whose clock is wrong; the poker copes, but it is worth seeing.
	ClockOffset = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hipo_poker_clock_offset_seconds",
		Help: "Chain time minus host time, in seconds.",
	})
)

func init() {
	LastReadSuccess.Set(float64(time.Now().Unix()))
	TreasuryStateFieldsExpected.Set(treasuryStateMinFields)
	BlindModeSince.Set(0)
}

// PublishOutstanding replaces the unconfirmed-poke series with the current set. The vector is
// reset rather than updated so that a confirmed poke's series disappears instead of ageing
// forever - the same trick gauge uses for per-round series, and what makes every alert built on
// it self-resolve.
func PublishOutstanding(outstanding map[Poke]time.Duration) {
	UnconfirmedPoke.Reset()
	for p, age := range outstanding {
		UnconfirmedPoke.WithLabelValues(p.Op.String(), strconv.FormatUint(uint64(p.RoundSince), 10)).
			Set(age.Seconds())
	}
}
