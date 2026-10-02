package poke

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

const testTreasury = "EQCLyZHP4Xe8fpchQz76O-_RmUhaVc_9BAoGyJrwJrcbz2eZ"

// TestTonapiSubmitsTheWholeExternal: tonapi takes a complete message, not a body, so what it gets
// has to be an external addressed to the treasury carrying exactly the body the liteservers get.
// Posting the body alone would be refused every time, and only the "refused" counter would say so.
func TestTonapiSubmitsTheWholeExternal(t *testing.T) {
	treasury := address.MustParseAddr(testTreasury)
	body := Body(Poke{Op: OpFinishParticipation, RoundSince: prevRound}, QueryID(uint32(testNow)))

	var got *cell.Cell
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("got %v with Content-Type %q, want a JSON POST", r.Method, r.Header.Get("Content-Type"))
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct{ Boc string }
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("request is not {\"boc\": ...}: %s", raw)
		}
		boc, err := base64.StdEncoding.DecodeString(req.Boc)
		if err != nil {
			t.Errorf("boc is not base64: %v", err)
		}
		got, err = cell.FromBOC(boc)
		if err != nil {
			t.Errorf("boc does not parse: %v", err)
		}
	}))
	defer server.Close()

	result, why := newTonapi(server.URL).submit(context.Background(), treasury, body)
	if result != tonapiAccepted || why != "" {
		t.Fatalf("a 200 came back as %v (%v)", result, why)
	}
	var msg tlb.Message
	if err := tlb.LoadFromCell(&msg, got.BeginParse()); err != nil {
		t.Fatalf("submitted cell is not a message: %v", err)
	}
	ext, ok := msg.Msg.(*tlb.ExternalMessage)
	if !ok {
		t.Fatalf("submitted a %T, want an external message", msg.Msg)
	}
	if !ext.DstAddr.Equals(treasury) {
		t.Fatalf("addressed to %v, want the treasury", ext.DstAddr)
	}
	if string(ext.Body.Hash()) != string(body.Hash()) {
		t.Fatal("the body tonapi got is not the body the liteservers get")
	}
}

// TestTonapiResults: an HTTP answer that is not 2xx is tonapi refusing - which is what it does
// with every early poke the treasury throws on - and no answer at all is the path being down. The
// two have to stay apart, because only the second says anything is wrong.
func TestTonapiResults(t *testing.T) {
	treasury := address.MustParseAddr(testTreasury)
	body := Body(Poke{Op: OpFinishParticipation, RoundSince: prevRound}, 1)

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"failed to send message: exitcode=205"}`, http.StatusInternalServerError)
	}))
	defer refusing.Close()
	if result, why := newTonapi(refusing.URL).submit(context.Background(), treasury, body); result != tonapiRefused || why == "" {
		t.Fatalf("a 500 came back as %v (%q), want refused with its reason", result, why)
	}

	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := gone.URL
	gone.Close()
	if result, _ := newTonapi(url).submit(context.Background(), treasury, body); result != tonapiUnreachable {
		t.Fatalf("a closed server came back as %v, want unreachable", result)
	}

	if newTonapi("") != nil {
		t.Fatal("an empty URL must turn the path off")
	}
}
