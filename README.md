# Poker

Poker drives [Hipo](https://github.com/HipoFinance/contract)'s validation rounds forward.

## Why it exists

The treasury's participation state machine only advances when someone sends it one of three
external messages:

| message | moves a round |
| --- | --- |
| `participate_in_election` | `open` → `distributing` |
| `vset_changed` | `staked` → `validating`, then `validating` → `held` |
| `finish_participation` | `held` → `recovering` |

Nothing on chain sends them. Until now the only senders were borrower-operated machines, so
whenever those stopped the protocol stopped with them: deferred unstakes missed the time the pool
promised, and a round's `vset_changed` could be lost entirely. That is not hypothetical — the
comment above `get_treasury_state` in `treasury.fc` records an occasion when it cost a round and
left a stake in the elector six hours past `stake_held_until`.

Poker is the driver of last resort. Borrowers keep poking; this is redundancy, not a replacement.

## What it is not

**It holds no wallet and no key.** All three ops are unsigned externals and the treasury calls
`accept_message()` itself, so the sender pays no gas and signs nothing. A fully compromised poker
host can do nothing that an anonymous stranger could not already do.

That is also why the governor retries — `retry_distribute`, `retry_recover_stakes`,
`retry_burn_all`, `retry_mint_bill` — are deliberately out of scope. They are internal messages
gated on `governor | halter`, and automating them would mean a hot governance key on an always-on
box.

## How it behaves

**It pokes at the first opportunity.** Each transition has a computable moment at which it becomes
legal, so the loop sleeps until that moment rather than polling towards it, and opens a one-second
burst from two seconds before until three after. Deadlines are measured on the **chain's** clock,
read from a liteserver, because the treasury's guards compare against a block's `gen_utime` and a
host clock a few seconds fast would fire early on every round forever.

**Over-poking is free.** Every guard in the treasury runs *before* `accept_message()`, so a
mistimed external is discarded in the compute phase with no transaction committed and nobody
charged. That is what makes the burst, two instances and duplicate borrower pokes all cost nothing.

**A successful send proves nothing.** A liteserver accepting the bytes says only that. A poke is
treated as outstanding until the state actually stops asking for it, and
`hipo_poker_unconfirmed_poke_seconds` is the series that says the protocol is not moving.

**A bad read degrades to blind, never to wrong.** The participations dictionary is read from a
positional index of `get_treasury_state`. That index has moved before, and it broke the borrower
tool. So the tuple's length, the type at every index touched, and the exact consumption of every
participation cell are all checked before anything is believed; a mismatch is a read failure, and
a read failure is **blind mode**, which derives candidate rounds from the network config alone and
fires everything plausible, parsing nothing the treasury returns.

**It respects a halt.** With `stopped? == true` it withholds `participate_in_election` and keeps
settling, so a halted pool does not keep lending but in-flight rounds still complete. Blind mode
cannot see `stopped?`, so it keeps participating for two hours — with an alert from the first
minute — and then withdraws.

## Configuration

| variable | meaning |
| --- | --- |
| `TREASURY_ADDRESS` | required |
| `OWN_LITESERVERS` | `host:port@base64key`, comma-separated. Tried first. |
| `GLOBAL_CONFIG_URL` | public liteserver pool, default `https://ton.org/global.config.json` |
| `METRICS_PORT` | default `10000` |
| `DRY_RUN` | compute and log every poke, send none |

Both liteserver sources are optional individually but not together. Own nodes are preferred
because they are closer and answer faster, which is what matters at a deadline; the public pool is
there because own nodes share a failure domain with the validators, and validators going down is
one of the cases this service exists to survive.

## Metrics

`hipo_poker_last_read_success_seconds`, `hipo_poker_blind_mode`,
`hipo_poker_blind_mode_since_seconds`, `hipo_poker_unconfirmed_poke_seconds{op,round_since}`,
`hipo_poker_pokes_sent_total{op}`, `hipo_poker_poke_errors_total{op}`,
`hipo_poker_confirmed_transitions_total{op}`, `hipo_poker_treasury_state_fields`,
`hipo_poker_treasury_state_fields_expected`, `hipo_poker_clock_offset_seconds`.

There is deliberately no per-round participation state here: `gauge` already publishes
`hipo_treasury_participation_state` and the round-lifecycle alerts are built on it. A second
publisher of the same fact would only create a way for the two to disagree.

## Develop

```sh
make test   # go build, go vet, go test
```

The tests need no chain access. Alerting rules and the deployment live in
[`HipoFinance/operation`](https://github.com/HipoFinance/operation) (`stack/poker.yaml`,
`monitor/rules/poker-alerting-rule.yaml`); the design is
`contract/docs/specs/2026-09-18-poke-service.md`.
