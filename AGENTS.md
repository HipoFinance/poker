# poker

## What this is

Always-on service that sends Hipo's three treasury externals — `participate_in_election`,
`vset_changed`, `finish_participation` — at the first second the contract will accept them.
Read `README.md` first; the design and the reasoning behind every choice are in
`HipoFinance/contract`'s `docs/specs/2026-09-18-poke-service.md`.

Orchestration/delegation policy is global (`~/.claude/CLAUDE.md`) — this file holds only project
facts.

### Stack
- Go 1.24, `tonutils-go` for chain access, Prometheus client for metrics. No Redis, no database,
  no wallet, no key.
- Deployed as a Docker image on Docker Swarm, two instances.

### Layout
- `main.go` — env config, signal handling, metrics server
- `poke/op.go`, `poke/participation.go` — the three ops, and the participation layout
- `poke/chain.go` — liteserver endpoints (own first, public fallback), network config, sending
- `poke/state.go` — the **guarded** `get_treasury_state` read
- `poke/due.go` — which pokes are due, the halt guard, the blind window, confirmation tracking
- `poke/clock.go` — chain-clock offset and the burst schedule
- `poke/send.go`, `poke/metrics.go`, `poke/run.go`

## Verify

```sh
go build ./... && go vet ./... && go test ./... && gofmt -l .
```

The suite is mutation-checked: removing the halt guard, disabling the blind-window expiry,
changing the message encoding, disabling the participation consumption check, or ignoring the
clock correction each fail a named test. If a change makes a test pass that should not, that is
the bug.

## Deploy

### After you push: move the pin in `operation`

A push to `main` here builds one image, tagged `sha-<short commit>`
(`.github/workflows/build.yml`). The deployment lives in
[`HipoFinance/operation`](https://github.com/HipoFinance/operation), which pins that tag in
`stack/poker.yaml`.

**Publishing an image changes nothing on the servers.** Moving the pin is a second step, and a
person deploys after it. The procedure is the `team:release` skill of the team plugin
(`/team:release`). In a session without the plugin, read the same text at
<https://github.com/HipoFinance/claude-team/blob/main/plugins/team/skills/release/SKILL.md>
(`gh api repos/HipoFinance/claude-team/contents/plugins/team/skills/release/SKILL.md -H "Accept: application/vnd.github.raw"`)
and follow it.

## Easy to get wrong

**Never widen a read into a guess.** `parseTreasuryState` exists to reject, not to parse. If the
tuple does not match the shape this build was written against, the answer is a `ShapeError` —
which becomes blind mode at once — and never a partially trusted value. A getter insert broke the
borrower tool in 2026-09-06 and cost a round; this service is the thing that is supposed to still
work then.

**But "I could not read it" is not "I do not believe it".** Everything that is not a `ShapeError`
is the chain being unavailable, and the two need opposite responses. An unavailable chain is
retried on the next endpoint (`readAcross` in `poke/run.go`) and then waited out for
`BlindTransportGrace` before blind mode starts. Collapsing the two is how a liteserver that was
43 blocks behind for one cycle put the service into blind mode twice in ten minutes on
2026-09-20 — 27 externals a time, and the halt guard armed off for two hours — while the public
pool in the same process was healthy throughout.

**`participate_in_election` is the only op with a safety question attached.** Settling ops
(`vset_changed`, `finish_participation`) run in every mode, always: withholding them strands
in-flight rounds and neither lends anything. Participating is different, because *what it does
depends on when it is sent*: inside the election window `distribute` lends, and once
`elected? | too_late?` holds it refunds every request and retires the round instead. The halt
policy is built on that distinction, not on withholding — see `participateDue` in `poke/due.go`.
Do not collapse those branches, and do not "fix" the stopped case by skipping the message: that
strands the collateral of borrowers who bid before the halt.

**A refusal is not a failure.** The treasury throwing before `accept_message` is what this
service is built on, and it surfaces as a liteserver error that looks like a transport failure —
in *two* shapes, because endpoints disagree: code `-701` with `exitcode=NNN`, and code `0` with
the bare sentence *external message was not accepted*. Match on the code alone and the second one
warns, counts as an error and drops its poke from the burst, which is what happened on
2026-09-23. `asRejection` in `poke/reject.go` separates them, and three things hang off that: only a real
transport failure warns, only a real transport failure counts in
`hipo_poker_poke_errors_total` (which `PokerNotSending` alerts on — conflating them paged about
every round), and a refused poke still counts as **sent**, so the burst keeps its cadence and the
tracker keeps ageing it.

**A node's verdict outranks a failure to hear from another node.** `Send` fans out to every
endpoint, and when none accepts, `chooseFailure` has to pick what to report. A duplicate or a
refusal is a node saying it has the message and ran it; a timeout is an endpoint saying nothing.
Reporting the last error to arrive reports the slowest endpoint, so a hung public pool turned an
ordinary `206` into `Failed to send ... context deadline exceeded` — warned, counted as an error,
dropped from the burst — 14 and 17 times in eleven days before 2026-10-07. The same reasoning bounds
the wait: once any node has answered, `collect` gives the others one tick (`answerGrace`), not the
whole `sendTimeout`, or one hung endpoint costs every send in a burst five seconds. With no verdict
at all it still waits for everyone, because then "nothing left" is the only honest report.

**A deadline does not need a participation.** Every deadline but one is read off a round the
treasury holds, and a round is only held once someone has bid. Bids are sealed and land a second
before the window, so "no round on the books" is the normal state right up to the deadline that
matters most. `unopenedRoundDeadline` in `poke/due.go` makes `participate_since` a deadline from
`get_times` alone, held for `ParticipateGrace` past its time like the rotation is. When adding a
deadline, ask what the loop does when the thing it is read from is not there yet.

**A read can be behind the chain, and a refusal can say so.** A read pins the last masterchain
block; a send is run against the node's newest shard state, and the treasury is on basechain. So a
cycle can read a round as due, send, and be told the round has already moved past that op
(`MovedPast`: 202, 204, 207, 7). `rereadIfStale` then reads again at the tick instead of treating
the poke as outstanding for a minute. It is bounded at `maxStaleRereads` in a row, because an
endpoint with stuck reads and live sends would otherwise loop at one second forever, and it is off
in blind mode, where `round_not_found` is just a wrong guess.

**`Expected` and `Settled` are different questions over the same codes, and they disagree.**
`Expected` decides whether to warn a human; `Settled` decides whether the burst stops re-sending,
which matters because the burst reads nothing between attempts and has only the exit code to go
on. 206 is expected and unsettled — `vset_changed` throws `vset_not_changed` both *before* a
rotation and *after* a successful one, since the handler packs `new_vset_hash` back into the
participation, so it cannot mean "done" on its own. An unknown code is unexpected and settled, so
meeting something new stops the loop rather than feeding it. Do not collapse the two predicates.

Two more wear a failure's clothes. **Exit code 7** is the treasury having no such round — all
three handlers `udict_get` the participation and hand a miss to `unpack_participation`. It is
ordinary in both modes: blind mode guesses rounds, and a sighted cycle sends from a read that is
seconds old, so a round that recovered in between is already gone. It warned once, briefly, on the
theory that it might mean participations had stopped unpacking — but that fails the treasury read
first and shows up as blind mode, so the warning only ever fired on the benign case.
**`duplicate message`** is a node saying it already holds this external,
which is delivery: the two instances build identical bodies on purpose, and the collision is free
deduplication, so do not salt the query id per instance to make it go away.

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

## When you change X, also update Y

### Run-book pages

Run-book pages in `HipoFinance/runbook` tell a person what to do when an alert or a bot message
arrives, and they depend on code here. **After every change to code in this repo, whatever its
size, run this before you report**, without being asked:

    python3 ../runbook/checks/sync.py --touched .

It only reads files, and it needs a clone of the run-book repo beside this one. For each page it
lists, read the page against your change. Your report says which pages depend on the changed
files and whether each needs an edit, or that the command listed none. If you could not run it,
say so. Editing a page follows the `team:runbook` skill of the team plugin (`/team:runbook`); a
page shown as marked needs its owner's review and a money review before it changes. In a session
without the plugin, read the same text at
<https://github.com/HipoFinance/claude-team/blob/main/plugins/team/skills/runbook/SKILL.md>
(`gh api repos/HipoFinance/claude-team/contents/plugins/team/skills/runbook/SKILL.md -H "Accept: application/vnd.github.raw"`)
and follow it.
