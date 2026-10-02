package poke

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// DefaultTonapiMessageURL is where every poke is also submitted, alongside the liteservers.
//
// On 2026-10-01 both instances sent finish_participation for round 1790791432 from 21:23:21, the
// first second it was due, and every liteserver - our two and the public pool - took the bytes, one
// of them even answering that it already held the message. None of those copies reached a collator
// until 21:32:58, nine and a half minutes later, while the basechain produced blocks normally and
// the treasury would have accepted any of them. TON Core traced it to a fault in the public overlay.
// sealed-borrower met the same thing on 2026-09-26 and has submitted through tonapi, which runs its
// own nodes, ever since; its requests have landed on time since then.
const DefaultTonapiMessageURL = "https://tonapi.io/v2/blockchain/message"

// tonapiTimeout bounds one submission. It runs beside the liteserver sends rather than in front of
// them, so a slow answer delays nothing.
const tonapiTimeout = 5 * time.Second

// tonapi submits externals to one HTTP endpoint. It is an extra path and nothing more: the result
// never reaches Send's caller, never counts as sent or as an error, and never confirms anything.
// That separation is deliberate. tonapi answers an external the treasury refuses - the ordinary
// case for an early poke - with an HTTP error in a shape of its own, and folding that into
// PokeErrors would page PokerNotSending about every round, which is the mistake reject.go exists to
// prevent for the liteservers' two shapes.
type tonapi struct {
	url    string
	client *http.Client

	mu   sync.Mutex
	last string
}

func newTonapi(url string) *tonapi {
	if url == "" {
		return nil
	}
	return &tonapi{url: url, client: &http.Client{Timeout: tonapiTimeout}}
}

// Results of one submission, as the label on TonapiSubmits.
const (
	tonapiAccepted    = "accepted"
	tonapiRefused     = "refused"
	tonapiUnreachable = "unreachable"
)

// submit posts one external and returns how it went and, when it did not go, why. It does not log;
// observe does.
func (t *tonapi) submit(ctx context.Context, treasury *address.Address, body *cell.Cell) (string, string) {
	msg, err := tlb.ToCell(&tlb.ExternalMessage{DstAddr: treasury, Body: body})
	if err != nil {
		return tonapiUnreachable, fmt.Sprintf("serialising the message: %v", err)
	}
	payload, err := json.Marshal(map[string]string{"boc": base64.StdEncoding.EncodeToString(msg.ToBOC())})
	if err != nil {
		return tonapiUnreachable, err.Error()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(payload))
	if err != nil {
		return tonapiUnreachable, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return tonapiUnreachable, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return tonapiAccepted, ""
	}
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	return tonapiRefused, fmt.Sprintf("%v: %s", resp.Status, strings.TrimSpace(string(text)))
}

// observe counts a result and logs it only when it differs from the previous one. A burst submits
// every second, and a line per submission would bury the sends and confirmations the log is for.
func (t *tonapi) observe(result, why string) {
	TonapiSubmits.WithLabelValues(result).Inc()
	t.mu.Lock()
	changed := result != t.last
	t.last = result
	t.mu.Unlock()
	if !changed {
		return
	}
	if why == "" {
		log.Printf("📨 tonapi is taking pokes")
	} else {
		log.Printf("ℹ️  tonapi did not take a poke (%v): %v", result, why)
	}
}

// submitDetached submits in the background. Send returns on the first liteserver that takes the
// message and must not wait on this, and the cycle's context may end as soon as it does, so the
// submission gets a context of its own.
func (t *tonapi) submitDetached(ctx context.Context, treasury *address.Address, body *cell.Cell) {
	go func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tonapiTimeout)
		defer cancel()
		t.observe(t.submit(sctx, treasury, body))
	}()
}
