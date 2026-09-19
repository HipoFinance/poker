package poke

import (
	"context"
	"log"
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
}

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
		log.Printf("💤 Next cycle in %v (%v)", wait.Round(time.Millisecond), reason)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Cycle is one read-decide-send pass. It never returns an error: every failure has a defined
// degraded behaviour, and a poke service that exits on a bad read is worse than useless.
func (p *Poker) Cycle(ctx context.Context) (time.Duration, string) {
	session, err := p.chain.Session(ctx)
	if err != nil {
		// Nothing was observed this cycle, so nothing may be claimed. Leaving the last ages
		// published would freeze them at whatever they were: a poke that happened to be one
		// retry old when the chain went away would then fire PokerPokeUnconfirmed for the whole
		// outage, with a description about the treasury refusing a message nobody could send.
		// PokerReadFailing is the alert for this.
		PublishOutstanding(nil)
		log.Printf("❌ No liteserver answered: %v", err)
		return RetryInterval, "no endpoint"
	}

	ReadBlockSeqno.Set(float64(session.Block.SeqNo))

	if chainNow, err := session.Now(session.Ctx); err != nil {
		log.Printf("⚠️  Could not read the chain clock: %v", err)
	} else {
		p.clock.Observe(chainNow)
		ClockOffset.Set(float64(p.clock.Offset()))
		ClockSynced.Set(1)
		LastClockSuccess.Set(float64(time.Now().Unix()))
	}

	network, err := session.NetworkConfig(session.Ctx)
	if err != nil {
		// Without the validator sets there are no candidate rounds and no rotation time, so not
		// even blind mode has anything to aim at. This is "cannot reach the chain", which is a
		// different failure from "cannot trust the treasury" and is reported by
		// hipo_poker_last_read_success_seconds ageing rather than by blind mode.
		PublishOutstanding(nil)
		log.Printf("❌ Could not read the network config: %v", err)
		return RetryInterval, "no network config"
	}

	// Everything below this line is reachable even when the treasury cannot be read, so the read
	// clock advances here.
	LastReadSuccess.Set(float64(time.Now().Unix()))

	treasury, readErr := session.ReadTreasury(session.Ctx)
	// Published whenever a tuple came back at all, including when it was then rejected - a shape
	// change is exactly the case where an operator needs to see the observed length, and
	// PokerBlindMode's runbook entry tells them to compare it against the expected one.
	// Fields is zero only when the get-method call itself failed.
	if treasury.Fields > 0 {
		TreasuryStateFields.Set(float64(treasury.Fields))
	}
	p.setBlind(readErr)

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
	sent := p.send(ctx, due)

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

func (p *Poker) send(ctx context.Context, due []Poke) []Poke {
	var sent []Poke
	for _, poke := range due {
		if p.dryRun {
			log.Printf("🧪 Would send %v", poke)
			continue
		}
		body := Body(poke, QueryID(p.clock.Now()))
		if err := p.chain.Send(ctx, body); err != nil {
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
	return sent
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
	if view.Blind {
		log.Printf("🙈 Blind for %v, candidate rounds %v, %d pokes due",
			time.Since(view.BlindSince).Round(time.Second), view.Network.Candidates(), len(due))
		return
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
	for _, round := range view.Treasury.Rounds() {
		part := view.Treasury.Participations[round]
		log.Printf("ℹ️  Round %v (%v): %v", round, time.Unix(int64(round), 0).Format(TimeFormat), part.State)
	}
	log.Printf("ℹ️  %d round(s), %d poke(s) due%v", len(view.Treasury.Participations), len(due), halted)
}

// TimeFormat matches the borrower tool's, so the two services' logs read the same way.
const TimeFormat = "Jan 2 15:04 -0700"
