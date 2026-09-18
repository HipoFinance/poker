// Poker drives Hipo's validation rounds forward.
//
// The treasury's participation state machine only advances when someone sends it one of three
// external messages: participate_in_election, vset_changed or finish_participation. Nothing on
// chain sends them. Until now the only senders were borrower-operated machines, so whenever those
// stopped the protocol stopped with them - unstakes missed the time the pool promised, and a
// round's vset_changed could be lost entirely.
//
// This service is the driver of last resort. It holds no wallet and no key: all three ops are
// unsigned externals and the treasury calls accept_message itself, so a compromised poke host can
// do nothing an anonymous stranger could not already do. Its whole job is to send those three
// messages at the first second the contract will accept them, and every minute after that until
// the state actually moves.
//
// See contract/docs/specs/2026-09-18-poke-service.md.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/xssnick/tonutils-go/address"

	"poker/poke"
)

func main() {
	log.Println("🟢 Poker started")

	treasuryAddress := mustEnv("TREASURY_ADDRESS")
	treasury, err := address.ParseAddr(treasuryAddress)
	if err != nil {
		log.Fatalf("❌ TREASURY_ADDRESS %q is not an address: %v", treasuryAddress, err)
	}

	opts := poke.Options{
		Treasury:        treasury,
		OwnServers:      parseLiteServers(os.Getenv("OWN_LITESERVERS")),
		GlobalConfigURL: envOr("GLOBAL_CONFIG_URL", "https://ton.org/global.config.json"),
		DryRun:          envBool("DRY_RUN"),
	}
	if opts.DryRun {
		log.Println("🧪 DRY_RUN is set: every poke will be computed and logged, and none will be sent")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go serveMetrics(envOr("METRICS_PORT", "10000"))

	poker, err := poke.New(ctx, opts)
	if err != nil {
		log.Fatalf("❌ %v", err)
	}

	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		s := <-stop
		log.Printf("❗️ Got signal '%v', stopping", s)
		cancel()
	}()

	poker.Run(ctx)
	log.Println("🔴 Poker stopped")
}

func serveMetrics(port string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("📈 Metrics on :%v/metrics", port)
	if err := server.ListenAndServe(); err != nil {
		log.Printf("⚠️  Metrics server stopped: %v", err)
	}
}

// parseLiteServers reads a comma-separated list of `host:port@base64key` entries. Empty is
// allowed and means "public liteservers only", which is a working configuration.
func parseLiteServers(s string) []poke.LiteServer {
	var out []poke.LiteServer
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		addr, key, ok := strings.Cut(entry, "@")
		if !ok {
			log.Printf("⚠️  Ignoring liteserver %q: expected host:port@base64key", entry)
			continue
		}
		out = append(out, poke.LiteServer{Addr: addr, Key: key})
	}
	return out
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("❌ %v is required", name)
	}
	return v
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envBool(name string) bool {
	v, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && v
}
