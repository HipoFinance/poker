package poke

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xssnick/tonutils-go/address"
)

type Options struct {
	Treasury        *address.Address
	OwnServers      []LiteServer
	GlobalConfigURL string
	// DryRun computes and logs what would be sent and sends nothing. It exists for the rehearsal
	// the spec asks for before the first deploy: run a full round against mainnet and diff what
	// this would have sent against what the borrowers actually sent.
	DryRun bool
}

type Poker struct {
	chain   *Chain
	clock   *Clock
	tracker *Tracker
	dryRun  bool

	blind blindState

	// Logging is on change, not on cycle. The loop wakes every five minutes at rest and every
	// second while it watches a deadline, and repeating an unchanged picture at either rate
	// buries the lines that matter - a send, a confirmation, entering blind mode - in noise. So
	// a view is logged when it differs from the last one, and otherwise at a slow heartbeat, so
	// that silence still means something.
	lastView   string
	lastWait   string
	lastLogged time.Time
}

// logHeartbeat is how often an unchanged picture is repeated anyway, so that a quiet log is
// distinguishable from a stopped process.
const logHeartbeat = 30 * time.Minute

func New(ctx context.Context, o Options) (*Poker, error) {
	chain, err := NewChain(ctx, o.Treasury, o.OwnServers, o.GlobalConfigURL)
	if err != nil {
		return nil, err
	}
	if o.DryRun {
		DryRun.Set(1)
	}
	return &Poker{
		chain:   chain,
		clock:   &Clock{},
		tracker: NewTracker(),
		dryRun:  o.DryRun,
	}, nil
}

// Run pokes until the context is cancelled.
func (p *Poker) Run(ctx context.Context) {
	for {
		wait, reason := p.Cycle(ctx)
		if wait < BurstTick {
			wait = BurstTick
		}
		if line := fmt.Sprintf("💤 Next cycle in %v (%v)", wait.Round(time.Millisecond), reason); line != p.lastWait {
			p.lastWait = line
			log.Print(line)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// reading is one cycle's view of the chain: one endpoint, one masterchain block, and everything
// read at it. err is the treasury read's failure, if any; the rest is still usable when it is set,
// which is what blind mode runs on.
type reading struct {
	session  *Session
	network  NetworkConfig
	treasury TreasuryState
	err      error
}

// read returns the first endpoint that could answer, trying them in configured order.
//
// The retry is over the whole reading rather than the one failing call, because a Session pins a
// masterchain block and the usual reason an endpoint fails a read at that block is that it does
// not have it yet. On 2026-09-20 one of our own liteservers reported a current block its own
// shard client was 43 behind, and every read against that block came back `is not in db`. Asking
// the same endpoint again would have failed identically; asking the next one, at the block IT is
// on, succeeds. Before this, a lagging node of ours put the service straight into blind mode
// while a healthy public pool sat unused in the same process.
func (p *Poker) read(ctx context.Context) (reading, error) {
	return readAcross(ctx, p.chain.Endpoints(), p.readFrom)
}

// readFrom is one endpoint's whole read. It returns an error only when the endpoint gave nothing
// usable at all; a reading whose `err` is set still carries the validator sets, which is what
// blind mode runs on.
func (p *Poker) readFrom(ctx context.Context, ep *Endpoint) (reading, error) {
	session, err := p.chain.SessionOn(ctx, ep)
	if err != nil {
		return reading{}, err
	}
	network, err := session.NetworkConfig(session.Ctx)
	if err != nil {
		return reading{}, err
	}
	treasury, readErr := session.ReadTreasury(session.Ctx)
	return reading{session: session, network: network, treasury: treasury, err: readErr}, nil
}

// readAcross walks the endpoints in order and takes the first usable answer.
//
// A ShapeError ends the search instead of continuing it: every endpoint returns the same tuple,
// so there is nothing to look for elsewhere, and that failure is the one blind mode was built
// for. A transport failure keeps looking, but the first endpoint that at least gave the validator
// sets is kept, so that a total failure of the treasury read still leaves blind mode something to
// aim at.
func readAcross(ctx context.Context, endpoints []*Endpoint,
	step func(context.Context, *Endpoint) (reading, error)) (reading, error) {

	var lastErr error
	var partial *reading

	for _, ep := range endpoints {
		r, err := step(ctx, ep)
		if err != nil {
			lastErr = fmt.Errorf("%v: %w", ep.Name, err)
			log.Printf("⚠️  endpoint %v did not answer: %v", ep.Name, err)
			continue
		}
		if r.err == nil || IsShapeError(r.err) {
			return r, nil
		}
		if partial == nil {
			partial = &r
		}
		lastErr = fmt.Errorf("%v: %w", ep.Name, r.err)
	}

	if partial != nil {
		partial.err = lastErr
		return *partial, nil
	}
	return reading{}, lastErr
}

// Cycle is one read-decide-send pass. It never returns an error: every failure has a defined
// degraded behaviour, and a poke service that exits on a bad read is worse than useless.
func (p *Poker) Cycle(ctx context.Context) (time.Duration, string) {
	r, err := p.read(ctx)
	if err != nil {
		// Nothing was observed this cycle, so nothing may be claimed. Leaving the last ages
		// published would freeze them at whatever they were: a poke that happened to be one
		// retry old when the chain went away would then fire PokerPokeUnconfirmed for the whole
		// outage, with a description about the treasury refusing a message nobody could send.
		// PokerReadFailing is the alert for this.
		PublishOutstanding(nil)
		log.Printf("❌ No endpoint could be read: %v", err)
		return RetryInterval, "no endpoint"
	}
	session, network, treasury, readErr := r.session, r.network, r.treasury, r.err

	ReadBlockSeqno.Set(float64(session.Block.SeqNo))

	if chainNow, err := session.Now(session.Ctx); err != nil {
		log.Printf("⚠️  Could not read the chain clock: %v", err)
	} else {
		p.clock.Observe(chainNow)
		ClockOffset.Set(float64(p.clock.Offset()))
		ClockSynced.Set(1)
		LastClockSuccess.Set(float64(time.Now().Unix()))
	}

	// The chain was reachable and the validator sets were read, which is what this series means.
	// Whether the treasury itself could be read is a different question, answered by
	// LastTreasuryRead below and by blind mode.
	LastReadSuccess.Set(float64(time.Now().Unix()))

	// Published whenever a tuple came back at all, including when it was then rejected - a shape
	// change is exactly the case where an operator needs to see the observed length, and
	// PokerBlindMode's runbook entry tells them to compare it against the expected one.
	// Fields is zero only when the get-method call itself failed.
	if treasury.Fields > 0 {
		TreasuryStateFields.Set(float64(treasury.Fields))
	}
	if readErr == nil {
		LastTreasuryRead.Set(float64(time.Now().Unix()))
	}
	p.setBlind(readErr)

	// A treasury read that has failed, but not for long enough to justify poking blind. Nothing
	// is known about any round this cycle, so nothing is poked and nothing is claimed; the grace
	// exists because the usual cause clears within a cycle or two.
	if readErr != nil && !p.blind.Blind() {
		PublishOutstanding(nil)
		log.Printf("⚠️  Could not read the treasury state, retrying (blind in %v): %v",
			p.blind.BlindIn(time.Now()).Round(time.Second), readErr)
		return waitWhileUnreadable(p.clock.Until(network.CurrentUntil)), "treasury unreadable"
	}

	wall := time.Now()
	view := View{
		Now:        p.clock.Now(),
		Network:    network,
		Blind:      p.blind.Blind(),
		BlindSince: p.blind.Since(),
		Treasury:   treasury,
	}

	due := Due(view, wall)
	p.logView(view, due)

	// Sending comes first so that only what actually left starts a clock. A poke no liteserver
	// accepted, or one a dry run merely logged, is not evidence that the treasury is refusing
	// anything, and treating it as such pages someone for a broken network path with a message
	// about a wedged round.
	sent := p.send(ctx, due, view.Blind)

	// Confirmation is measured against what the treasury would still accept, never against what
	// this service chose to send, so a halt taking effect mid-window cannot be mistaken for a
	// transition. Blind mode computes no such set and publishes no ages: it cannot confirm
	// anything, and PokerBlindMode is the signal for that.
	if !p.blind.Blind() {
		for _, c := range p.tracker.Observe(DueByContract(view), sent, wall) {
			log.Printf("✅ %v confirmed: the state moved", c)
			Confirmed.WithLabelValues(c.Op.String()).Inc()
		}
		PublishOutstanding(p.tracker.Outstanding(wall))
	} else {
		PublishOutstanding(nil)
	}

	return p.schedule(view, wall, len(due) > 0)
}

func (p *Poker) send(ctx context.Context, due []Poke, blind bool) []Poke {
	var sent []Poke

	// Blind mode's ordinary outcomes are collapsed into one line. Its candidate rounds are
	// guesses stepped back from the validator sets, so most of them are rounds the treasury has
	// never held and answer round_not_found; a cycle produces two dozen such lines and buries the
	// one that matters. A sighted cycle pokes only rounds it just read, at most a handful, and
	// every outcome there is worth its own line.
	routine := map[string]int{}
	report := func(reason, line string) {
		if blind {
			routine[reason]++
			return
		}
		log.Print(line)
	}

	for _, poke := range due {
		if p.dryRun {
			log.Printf("🧪 Would send %v", poke)
			continue
		}
		body := Body(poke, QueryID(p.clock.Now()))
		err := p.chain.Send(ctx, body)

		if isDuplicate(err) {
			// The message is at a node already, put there by the other instance or by an earlier
			// attempt in the same second. That is delivery, so it counts as sent for every
			// purpose: the burst keeps its cadence and the poke keeps ageing until the state
			// moves. Counting it as a failure is what made a whole blind cycle look like a total
			// outage in the log while every message was in fact on its way.
			report("already queued", fmt.Sprintf("👯 %v was already queued at a node", poke))
			PokesDuplicate.WithLabelValues(poke.Op.String()).Inc()
			sent = append(sent, poke)
			continue
		}

		if reject, ok := asRejection(err); ok {
			// The treasury refused it, which is the ordinary case and not a fault: the guards
			// run before accept_message, so a poke aimed a moment early or one whose work another
			// sender has already done is thrown away at nobody's expense.
			//
			// It counts as SENT even so. The message reached a node and was run against the
			// chain's state, which is what the tracker means by outstanding - so a poke that
			// keeps being refused keeps ageing towards PokerPokeUnconfirmed, and one refused
			// because the work is already done drops out of the due set on the next read and is
			// confirmed. It also keeps the burst cadence, which matters: a poke refused for being
			// a second early wants retrying in a second, not in a minute.
			if reject.Expected(blind) {
				report(reject.Reason(), fmt.Sprintf("↩️  %v refused: %v", poke, reject.Reason()))
			} else {
				log.Printf("⚠️  %v refused for an unexpected reason: %v", poke, reject.Reason())
			}
			PokesRejected.WithLabelValues(poke.Op.String(), strconv.Itoa(reject.Code)).Inc()
			sent = append(sent, poke)
			continue
		}

		if err != nil {
			// Nothing reached the chain. This is the one that deserves a warning, and the one
			// PokerNotSending is counting.
			log.Printf("⚠️  Failed to send %v: %v", poke, err)
			PokeErrors.WithLabelValues(poke.Op.String()).Inc()
			continue
		}

		// Deliberately not "sent successfully". A liteserver took the bytes; whether the treasury
		// accepts them is decided by a guard that leaves no receipt either way.
		log.Printf("📨 Sent %v", poke)
		PokesSent.WithLabelValues(poke.Op.String()).Inc()
		sent = append(sent, poke)
	}

	if len(routine) > 0 {
		log.Printf("🙈 Blind cycle: %v", summarise(routine))
	}
	return sent
}

// summarise renders the collapsed blind-mode outcomes, commonest first and alphabetically within
// a count, so that consecutive cycles produce the same line when nothing has changed - which is
// what logView's on-change suppression needs to be able to stay quiet.
func summarise(counts map[string]int) string {
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	sort.Slice(reasons, func(i, j int) bool {
		if counts[reasons[i]] != counts[reasons[j]] {
			return counts[reasons[i]] > counts[reasons[j]]
		}
		return reasons[i] < reasons[j]
	})

	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%v ×%d", reason, counts[reason]))
	}
	return strings.Join(parts, ", ")
}

// waitWhileUnreadable is the sleep for a cycle that could not read the treasury: the plain retry,
// except that it will not sleep through a validator-set rotation.
//
// Nothing is known about any round without the participations dictionary, so there is no deadline
// to aim at - with one exception, and it is the one that costs the most to miss. The rotation is
// read from config 34, which is still readable, and it is the moment vset_changed becomes legal
// for every validating round. Sleeping a flat minute through it would hand back the lateness the
// burst exists to remove, for a read that may well succeed by then. So the cycle lands just
// before the rotation and retries at the burst tick across it; a second or two either side, then
// back to the minute.
func waitWhileUnreadable(untilRotation time.Duration) time.Duration {
	wait := RetryInterval
	if untilRotation <= 0 {
		return wait
	}
	if toDeadline := untilRotation - BurstLead; toDeadline < wait {
		if toDeadline < BurstTick {
			toDeadline = BurstTick
		}
		wait = toDeadline
	}
	return wait
}

func (p *Poker) schedule(view View, wall time.Time, pending bool) (time.Duration, string) {
	if p.blind.Blind() {
		// No deadlines are known and nothing can be confirmed, so there is nothing to burst
		// towards and nothing to chase. Plain retries until the read comes back.
		return RetryInterval, "blind"
	}
	// The NEWEST, not the oldest: this answers "did we send something a moment ago", and using
	// the oldest let one wedged round switch off the burst for every other round.
	// `sent` is threaded through rather than inferred: an empty tracker reports an age of zero,
	// which NextWait would otherwise read as "sent this instant" and answer with a one-second
	// cadence, forever.
	var age time.Duration
	_, newest, sent := p.tracker.Newest(wall)
	if sent {
		age = newest
	}
	deadline, haveDeadline := NextDeadline(view)
	var until time.Duration
	if haveDeadline {
		until = p.clock.Until(deadline)
	}
	return NextWait(pending, sent, age, until, haveDeadline)
}

func (p *Poker) setBlind(readErr error) {
	entered, left := p.blind.Observe(readErr, time.Now())
	if entered {
		BlindEntries.Inc()
		log.Printf("🙈 Blind mode: %v", readErr)
		log.Printf("🙈 Poking every candidate round from the network config. "+
			"participate_in_election is included for the next %v, then withdrawn.",
			BlindParticipateWindow)
	}
	if left {
		log.Printf("👁  Treasury state is readable again")
	}
	if p.blind.Blind() {
		BlindMode.Set(1)
		BlindModeSince.Set(float64(p.blind.Since().Unix()))
		return
	}
	BlindMode.Set(0)
	BlindModeSince.Set(0)
}

func (p *Poker) logView(view View, due []Poke) {
	lines := viewLines(view, due)
	summary := strings.Join(lines, "\n")

	if summary == p.lastView && time.Since(p.lastLogged) < logHeartbeat {
		return
	}
	p.lastView = summary
	p.lastLogged = time.Now()
	for _, line := range lines {
		log.Print(line)
	}
}

func viewLines(view View, due []Poke) []string {
	if view.Blind {
		return []string{fmt.Sprintf("🙈 Blind, candidate rounds %v, %d poke(s) due",
			view.Network.Candidates(), len(due))}
	}

	var lines []string
	for _, round := range view.Treasury.Rounds() {
		part := view.Treasury.Participations[round]
		lines = append(lines, fmt.Sprintf("ℹ️  Round %v (%v): %v",
			round, time.Unix(int64(round), 0).Format(TimeFormat), part.State))
	}

	halted := ""
	if view.Treasury.Stopped {
		// Which of the two it is matters more than the halt itself: sending this message to a
		// halted pool is the most consequential thing this service ever does, and an operator
		// reading the log during an incident must not be told the opposite of what happened.
		halted = ", treasury STOPPED so participate_in_election waits for distribute's refund branch"
		for _, poke := range due {
			if poke.Op == OpParticipateInElection {
				halted = ", treasury STOPPED and participate_in_election IS being sent: " +
					"distribute will refund this round's requests, not stake them"
				break
			}
		}
	}
	return append(lines, fmt.Sprintf("ℹ️  %d round(s), %d poke(s) due%v",
		len(view.Treasury.Participations), len(due), halted))
}

// TimeFormat matches the borrower tool's, so the two services' logs read the same way.
const TimeFormat = "Jan 2 15:04 -0700"
