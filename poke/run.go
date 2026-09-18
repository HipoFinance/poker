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

	blind      bool
	blindSince time.Time
}

func New(ctx context.Context, o Options) (*Poker, error) {
	chain, err := NewChain(ctx, o.Treasury, o.OwnServers, o.GlobalConfigURL)
	if err != nil {
		return nil, err
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
		log.Printf("❌ No liteserver answered: %v", err)
		return RetryInterval, "no endpoint"
	}

	if chainNow, err := session.Now(session.Ctx); err != nil {
		log.Printf("⚠️  Could not read the chain clock: %v", err)
	} else {
		p.clock.Observe(chainNow)
		ClockOffset.Set(float64(p.clock.Offset()))
	}

	network, err := session.NetworkConfig(session.Ctx)
	if err != nil {
		// Without the validator sets there are no candidate rounds and no rotation time, so not
		// even blind mode has anything to aim at. This is "cannot reach the chain", which is a
		// different failure from "cannot trust the treasury" and is reported by
		// hipo_poker_last_read_success_seconds ageing rather than by blind mode.
		log.Printf("❌ Could not read the network config: %v", err)
		return RetryInterval, "no network config"
	}

	// Everything below this line is reachable even when the treasury cannot be read, so the read
	// clock advances here.
	LastReadSuccess.Set(float64(time.Now().Unix()))

	treasury, readErr := session.ReadTreasury(session.Ctx)
	if readErr == nil {
		TreasuryStateFields.Set(float64(treasury.Fields))
	}
	p.setBlind(readErr)

	wall := time.Now()
	view := View{
		Now:        p.clock.Now(),
		Network:    network,
		Blind:      p.blind,
		BlindSince: p.blindSince,
		Treasury:   treasury,
	}

	due := Due(view, wall)
	p.logView(view, due)

	// Confirmation only means something in precise mode. Blind mode has every op due for every
	// candidate on every cycle, so nothing ever leaves the due set and an age tracked there would
	// grow without bound and read as a wedge. PokerBlindMode is the signal for that.
	if !p.blind {
		for _, c := range p.tracker.Observe(due, wall) {
			log.Printf("✅ %v confirmed: the state moved", c)
			Confirmed.WithLabelValues(c.Op.String()).Inc()
		}
		PublishOutstanding(p.tracker.Outstanding(wall))
	} else {
		PublishOutstanding(nil)
	}

	p.send(ctx, due)

	return p.schedule(view, wall, len(due) > 0)
}

func (p *Poker) send(ctx context.Context, due []Poke) {
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
	}
}

func (p *Poker) schedule(view View, wall time.Time, pending bool) (time.Duration, string) {
	if p.blind {
		// No deadlines are known and nothing can be confirmed, so there is nothing to burst
		// towards and nothing to chase. Plain retries until the read comes back.
		return RetryInterval, "blind"
	}
	var age time.Duration
	if _, oldest, ok := p.tracker.Oldest(wall); ok {
		age = oldest
	}
	deadline, haveDeadline := NextDeadline(view)
	var until time.Duration
	if haveDeadline {
		until = p.clock.Until(deadline)
	}
	return NextWait(pending, age, until, haveDeadline)
}

func (p *Poker) setBlind(readErr error) {
	if readErr != nil {
		if !p.blind {
			p.blind = true
			p.blindSince = time.Now()
			p.tracker.Reset()
			log.Printf("🙈 Blind mode: %v", readErr)
			log.Printf("🙈 Poking every candidate round from the network config. "+
				"participate_in_election is included for the next %v, then withdrawn.",
				BlindParticipateWindow)
		}
		BlindMode.Set(1)
		BlindModeSince.Set(float64(p.blindSince.Unix()))
		return
	}
	if p.blind {
		p.blind = false
		p.tracker.Reset()
		log.Printf("👁  Treasury state is readable again")
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
		halted = ", treasury STOPPED so participate_in_election is withheld"
	}
	for _, round := range view.Treasury.Rounds() {
		part := view.Treasury.Participations[round]
		log.Printf("ℹ️  Round %v (%v): %v", round, time.Unix(int64(round), 0).Format(TimeFormat), part.State)
	}
	log.Printf("ℹ️  %d round(s), %d poke(s) due%v", len(view.Treasury.Participations), len(due), halted)
}

// TimeFormat matches the borrower tool's, so the two services' logs read the same way.
const TimeFormat = "Jan 2 15:04 -0700"
