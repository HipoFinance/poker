package poke

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// Network config parameter ids, as config::* in contracts/imports/constants.fc.
const (
	configPreviousValidators = 32
	configCurrentValidators  = 34
	configNextValidators     = 36
)

// Endpoint is one liteserver pool. Two are used, in order: the pool of our own nodes, and the
// public pool from ton.org's global config. Own nodes are preferred because they are closer and
// answer faster, which matters at a deadline; the public pool exists because a validator going
// down is one of the cases this service is meant to survive, and own nodes share that fate.
type Endpoint struct {
	Name string
	pool *liteclient.ConnectionPool
	api  ton.APIClientWrapped
}

type Chain struct {
	endpoints []*Endpoint
	treasury  *address.Address
}

// Session pins one cycle to one endpoint and one masterchain block, so every read in that cycle
// sees the same state. Mixing endpoints inside a cycle would let the participation set and the
// validator-set hash come from different moments, and the whole point of comparing them is that
// they are from the same one.
type Session struct {
	Endpoint *Endpoint
	Ctx      context.Context
	Block    *ton.BlockIDExt
	Treasury *address.Address
}

// NewChain dials the own-node addresses first and the public global config second. An endpoint
// that fails to dial at startup is dropped with a log line rather than being fatal: one working
// endpoint is enough to poke, and refusing to start because the slower one is down would be the
// wrong trade for a service whose job is to be up.
func NewChain(ctx context.Context, treasury *address.Address, ownServers []LiteServer, globalConfigURL string) (*Chain, error) {
	c := &Chain{treasury: treasury}

	if len(ownServers) > 0 {
		pool := liteclient.NewConnectionPool()
		added := 0
		for _, s := range ownServers {
			if err := pool.AddConnection(ctx, s.Addr, s.Key); err != nil {
				log.Printf("⚠️  own liteserver %v unavailable: %v", s.Addr, err)
				continue
			}
			added++
		}
		if added > 0 {
			c.endpoints = append(c.endpoints, &Endpoint{
				Name: "own",
				pool: pool,
				api:  ton.NewAPIClient(pool).WithRetry(3),
			})
		}
	}

	if globalConfigURL != "" {
		pool := liteclient.NewConnectionPool()
		if err := pool.AddConnectionsFromConfigUrl(ctx, globalConfigURL); err != nil {
			log.Printf("⚠️  public liteservers unavailable: %v", err)
		} else {
			c.endpoints = append(c.endpoints, &Endpoint{
				Name: "public",
				pool: pool,
				api:  ton.NewAPIClient(pool).WithRetry(3),
			})
		}
	}

	if len(c.endpoints) == 0 {
		return nil, fmt.Errorf("no liteserver endpoint could be reached")
	}
	return c, nil
}

// Session takes the first endpoint that answers, in configured order.
func (c *Chain) Session(ctx context.Context) (*Session, error) {
	var lastErr error
	for _, ep := range c.endpoints {
		sctx, cancel := context.WithTimeout(ep.pool.StickyContext(ctx), 10*time.Second)
		block, err := ep.api.CurrentMasterchainInfo(sctx)
		if err != nil {
			cancel()
			lastErr = err
			log.Printf("⚠️  endpoint %v did not answer: %v", ep.Name, err)
			continue
		}
		cancel()
		return &Session{
			Endpoint: ep,
			Ctx:      ep.pool.StickyContext(ctx),
			Block:    block,
			Treasury: c.treasury,
		}, nil
	}
	return nil, fmt.Errorf("no endpoint answered: %w", lastErr)
}

// Now is the liteserver's clock, which is the clock that decides whether a poke is early. See
// Clock in clock.go for why the host's own clock is not used for this.
func (s *Session) Now(ctx context.Context) (uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.Endpoint.api.GetTime(ctx)
}

// NetworkConfig is everything this service needs that does not come from the treasury. It is the
// part of a cycle that blind mode still relies on, so it is read separately and never mixed with
// the treasury read.
type NetworkConfig struct {
	CurrentVsetHash *big.Int
	CurrentSince    uint32
	CurrentUntil    uint32 // the next validator-set rotation, and the next round's round_since
	PreviousSince   uint32
	PreviousUntil   uint32
	NextSince       uint32 // zero outside the election window, when config 36 is absent
}

// blindReach is how many validator epochs before the previous set blind mode still aims at.
//
// request_loan lets the treasury hold up to eight participations, and only three of them are
// named by the validator sets themselves. A round that missed two rotations, or one left in `held`
// well past its stake_held_until, has a round_since older than config 32 - and those are precisely
// the incidents this service exists to resolve, so blind mode must not be unable to see them. The
// extra candidates are stepped back by the epoch length, which is a guess whenever the network's
// round length has changed, but a wrong guess costs nothing: the external is discarded before
// accept_message like any other.
const blindReach = 6

// Candidates is every round_since the treasury could plausibly still be holding, derived from the
// validator sets alone. This is what blind mode pokes at.
//
// The reach is what matters: a round in held or recovering carries the round_since of a set that
// has already rotated, and with an 18-hour round and a 9-hour hold that is at most the previous
// set. Config 32 therefore reaches one full set further back than anything that can still be
// waiting, and config 36 covers the round being elected. Ordered oldest first so that a round that
// has been waiting longest is poked first.
func (n NetworkConfig) Candidates() []uint32 {
	epoch := n.CurrentUntil - n.CurrentSince

	// Oldest first, so a round that has been waiting longest is poked first.
	var ordered []uint32
	if epoch > 0 && n.PreviousSince > 0 {
		for k := blindReach; k >= 1; k-- {
			step := uint32(k) * epoch
			if n.PreviousSince <= step {
				continue // before the chain began; nothing to aim at
			}
			ordered = append(ordered, n.PreviousSince-step)
		}
	}
	ordered = append(ordered, n.PreviousSince, n.PreviousUntil, n.CurrentSince, n.CurrentUntil, n.NextSince)

	seen := map[uint32]bool{}
	var out []uint32
	for _, v := range ordered {
		if v == 0 || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func (s *Session) NetworkConfig(ctx context.Context) (NetworkConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Always present on a running chain. A failure here is a real failure. Only what is actually
	// used is requested: any absent parameter fails the whole call, so asking for one this
	// service does not read would be a way to stop poking for no reason.
	required, err := s.Endpoint.api.GetBlockchainConfig(ctx, s.Block,
		configPreviousValidators, configCurrentValidators)
	if err != nil {
		return NetworkConfig{}, fmt.Errorf("blockchain config: %w", err)
	}

	// Present only while an election is open. Absent is the normal case, not an error.
	var next *cell.Cell
	if optional, err := s.Endpoint.api.GetBlockchainConfig(ctx, s.Block, configNextValidators); err == nil {
		next = optional.Get(configNextValidators)
	}

	return buildNetworkConfig(
		required.Get(configPreviousValidators),
		required.Get(configCurrentValidators),
		next,
	)
}

// buildNetworkConfig assembles the config from raw cells, separately from fetching them, so that
// an absent config 36 can be tested without a chain.
func buildNetworkConfig(previous, current, next *cell.Cell) (NetworkConfig, error) {
	var n NetworkConfig
	var err error

	if current == nil {
		return n, fmt.Errorf("config %d is absent", configCurrentValidators)
	}
	n.CurrentSince, n.CurrentUntil, err = vsetTimes(current)
	if err != nil {
		return n, fmt.Errorf("config %d: %w", configCurrentValidators, err)
	}
	// The treasury compares config_param(34).cell_hash() against the hash stored in the
	// participation, so the comparison here has to be over the same cell.
	n.CurrentVsetHash = new(big.Int).SetBytes(current.Hash())

	if previous != nil {
		n.PreviousSince, n.PreviousUntil, err = vsetTimes(previous)
		if err != nil {
			return n, fmt.Errorf("config %d: %w", configPreviousValidators, err)
		}
	}
	if next != nil {
		n.NextSince, _, err = vsetTimes(next)
		if err != nil {
			return n, fmt.Errorf("config %d: %w", configNextValidators, err)
		}
	}
	return n, nil
}

// vsetTimes reads utime_since and utime_until, following get_vset_times in imports/utils.fc:
//
//	validators_ext#12 utime_since:uint32 utime_until:uint32 ... = ValidatorSet;
func vsetTimes(c *cell.Cell) (uint32, uint32, error) {
	s := c.BeginParse()
	tag, err := s.LoadUInt(8)
	if err != nil {
		return 0, 0, fmt.Errorf("tag: %w", err)
	}
	// 0x12 only, which is what get_vset_times throws on anything else for
	// (err::unexpected_validator_set_format in imports/utils.fc). Accepting a shape the contract
	// rejects would have this service computing deadlines happily for a config the treasury will
	// not parse, and poking forever at a guard that can never pass.
	if tag != 0x12 {
		return 0, 0, fmt.Errorf("unexpected validator set tag 0x%02x", tag)
	}
	since, err := s.LoadUInt(32)
	if err != nil {
		return 0, 0, fmt.Errorf("utime_since: %w", err)
	}
	until, err := s.LoadUInt(32)
	if err != nil {
		return 0, 0, fmt.Errorf("utime_until: %w", err)
	}
	// Ordering is checked because Candidates() subtracts these to get an epoch length. An
	// inverted pair would underflow uint32 into roughly four billion, pass the `epoch > 0` test,
	// and have blind mode aiming at invented round numbers.
	if until <= since {
		return 0, 0, fmt.Errorf("validator set ends at %d, which is not after it starts at %d", until, since)
	}
	return uint32(since), uint32(until), nil
}

// sendTimeout bounds one endpoint's attempt. Short on purpose: a poke that has not left in five
// seconds has missed the block it was aimed at, and there is another attempt a second or a minute
// behind it.
const sendTimeout = 5 * time.Second

// Send broadcasts one external to every configured endpoint. Every endpoint is used rather than
// just the session's, because a duplicate external costs nothing - the treasury's guards run
// before accept_message, so at most one of them commits a transaction - and at a deadline the
// extra path is the difference between landing in this masterchain block and the next.
//
// It reports success if any endpoint accepted the bytes. Note that this says nothing about
// whether the treasury will accept the message: an external that fails a guard is discarded with
// no transaction and no receipt. Confirmation is a state re-read, in due.go.
func (c *Chain) Send(ctx context.Context, body *cell.Cell) error {
	// Endpoints in parallel, with a short timeout each. Serially, a single hanging endpoint would
	// add its full timeout to every poke in the cycle - and blind mode can have two dozen pokes -
	// which would stretch a sixty-second cycle into minutes and make the service miss the
	// deadlines it exists to hit. The sends are independent, so there is nothing to order.
	if len(c.endpoints) == 0 {
		// Unreachable today - NewChain refuses to build a Chain with none - but this function
		// must never report a send it did not make. The caller starts a clock on what it returns.
		return fmt.Errorf("no endpoints configured")
	}

	results := make(chan error, len(c.endpoints))
	for _, ep := range c.endpoints {
		go func(ep *Endpoint) {
			ectx, cancel := context.WithTimeout(ep.pool.StickyContext(ctx), sendTimeout)
			defer cancel()
			results <- ep.api.SendExternalMessage(ectx, &tlb.ExternalMessage{
				DstAddr: c.treasury,
				Body:    body,
			})
		}(ep)
	}

	// Read as they arrive and return on the first success rather than waiting for all of them.
	// Waiting made every send take the SLOWEST endpoint's time, so one black-holed endpoint still
	// added its whole timeout to every poke in a cycle - which is the problem making these
	// concurrent was supposed to solve, and making them concurrent alone did not. The channel is
	// buffered to the number of endpoints, so stragglers finish into it and nothing leaks.
	var lastErr error
	for range c.endpoints {
		if err := <-results; err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

// LiteServer is one own-node liteserver, as `host:port` plus its base64 public key.
type LiteServer struct {
	Addr string
	Key  string
}
