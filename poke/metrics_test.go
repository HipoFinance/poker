package poke

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestEveryCounterSeriesExistsBeforeItsFirstIncrement. A label set that is only created by its
// first increment is invisible to rate() and increase() for that increment, and for everything
// else before the first scrape. On 2026-09-23 the code="206" series appeared already at 5 and 10
// - the whole first rotation burst - and PokerNotSending, a rate over PokeErrors, could not see
// the first transport failure of any op after a restart.
//
// Against fresh vectors so the count is exact: the package's own are incremented by every other
// test in this binary.
func TestEveryCounterSeriesExistsBeforeItsFirstIncrement(t *testing.T) {
	perOp := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "per_op_total", Help: "t"}, []string{"op"})
	rejected := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "rejected_total", Help: "t"}, []string{"op", "code"})

	precreate(AllOps, knownCodes(), []*prometheus.CounterVec{perOp}, rejected)

	if got := testutil.CollectAndCount(perOp); got != len(AllOps) {
		t.Fatalf("%d per-op series exist before any increment, want %d", got, len(AllOps))
	}
	if got, want := testutil.CollectAndCount(rejected), len(AllOps)*len(knownCodes()); got != want {
		t.Fatalf("%d rejection series exist before any increment, want %d", got, want)
	}
	for _, op := range AllOps {
		if v := testutil.ToFloat64(perOp.WithLabelValues(op.String())); v != 0 {
			t.Fatalf("%v was created at %v, not 0", op, v)
		}
	}

	// And the refusal a node reports with no exit code has to be among them, or the shape that
	// cost a false warning on 2026-09-23 loses its first occurrence the same way.
	before := testutil.CollectAndCount(rejected)
	rejected.WithLabelValues(OpVsetChanged.String(), "0")
	if testutil.CollectAndCount(rejected) != before {
		t.Fatal(`code "0" was not pre-created; its first refusal would be invisible to increase()`)
	}
}

// And init actually does it for the vector the alert is built on. participate_in_election is the
// op to check because no other test in this package makes one of its sends fail, so its series
// exists only if init created it.
func TestInitCreatesTheSeriesPokerNotSendingReads(t *testing.T) {
	if got := testutil.CollectAndCount(PokeErrors); got != len(AllOps) {
		t.Fatalf("PokeErrors has %d series, want one per op (%d) from process start", got, len(AllOps))
	}
}
