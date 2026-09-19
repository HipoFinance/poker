# poker

Always-on service that sends Hipo's three treasury externals — `participate_in_election`,
`vset_changed`, `finish_participation` — at the first second the contract will accept them.
Read `README.md` first; the design and the reasoning behind every choice are in
`HipoFinance/contract`'s `docs/specs/2026-09-18-poke-service.md`.

Orchestration/delegation policy is global (`~/.claude/CLAUDE.md`) — this file holds only project
facts.

## Stack
- Go 1.24, `tonutils-go` for chain access, Prometheus client for metrics. No Redis, no database,
  no wallet, no key.
- Deployed as a Docker image on Docker Swarm, two instances.

## Layout
- `main.go` — env config, signal handling, metrics server
- `poke/op.go`, `poke/participation.go` — the three ops, and the participation layout
- `poke/chain.go` — liteserver endpoints (own first, public fallback), network config, sending
- `poke/state.go` — the **guarded** `get_treasury_state` read
- `poke/due.go` — which pokes are due, the halt guard, the blind window, confirmation tracking
- `poke/clock.go` — chain-clock offset and the burst schedule
- `poke/send.go`, `poke/metrics.go`, `poke/run.go`

## Things that are easy to get wrong here

**Never widen a read into a guess.** `parseTreasuryState` exists to reject, not to parse. If the
tuple does not match the shape this build was written against, the answer is an error — which
becomes blind mode — and never a partially trusted value. A getter insert broke the borrower tool
in 2026-09-06 and cost a round; this service is the thing that is supposed to still work then.

**`participate_in_election` is the only op with a safety question attached.** Settling ops
(`vset_changed`, `finish_participation`) run in every mode, always: withholding them strands
in-flight rounds and neither lends anything. Participating is different, because *what it does
depends on when it is sent*: inside the election window `distribute` lends, and once
`elected? | too_late?` holds it refunds every request and retires the round instead. The halt
policy is built on that distinction, not on withholding — see `participateDue` in `poke/due.go`.
Do not collapse those branches, and do not "fix" the stopped case by skipping the message: that
strands the collateral of borrowers who bid before the halt.

**A send is not a confirmation.** An external that fails a guard leaves no transaction and no
receipt. Anything that reports success on `SendExternalMessage` returning nil is wrong; the
`Tracker` and the state re-read are how a transition is established. Two rules keep that series
honest and both were got wrong once: only what actually **left** starts a clock (never what was
merely due, or logged by a dry run), and a poke is confirmed against `DueByContract` — what the
treasury would still accept — never against what this service chose to send.

**The burst is measured from the newest send, the alert from the oldest.** `schedule` feeds
`Tracker.Newest` into `NextWait`. It used to feed `Oldest`, which meant one wedged round put every
cycle on the plain retry interval and silently switched off the first-opportunity burst for every
other round. `TestScheduleUsesTheNewestSend` pins it at the wiring, because a unit test on
`NextWait` alone passed throughout.

**Timing is on the chain's clock.** Never schedule against `time.Now()` directly; go through
`Clock`.

## Verify before declaring done
```sh
go build ./... && go vet ./... && go test ./... && gofmt -l .
```

The suite is mutation-checked: removing the halt guard, disabling the blind-window expiry,
changing the message encoding, disabling the participation consumption check, or ignoring the
clock correction each fail a named test. If a change makes a test pass that should not, that is
the bug.

## After you push: bump the pin in `operation`

A push to `main` builds one image tagged `sha-<short commit>` (`.github/workflows/build.yml`).
The deployment lives in [`HipoFinance/operation`](https://github.com/HipoFinance/operation), which
pins that tag in `stack/poker.yaml`. Run `./bump.sh poker` there after the build finishes.
