# flaky-net-harness

Prove what a field-service app destroys on a bad mobile network — and prove it
with numbers a vendor cannot argue with.

A field engineer finishes a job in a basement plant room, taps submit, sees a
tick, and drives off. The photos never reached the server. Nobody finds out for
three days. That failure mode is common, it is invisible in the vendor's own
test suite, and it is what this tool exists to document.

`flaky-net-harness` does two things:

1. **Pins the network conditions.** Three named, versioned profiles built on
   `tc`/`netem` — sustained congestion, intermittent total stall, hard
   mid-transfer disconnect — each with concrete bandwidth, latency, loss and
   jitter figures, plus adversarial events (kill the app mid-upload, drop the
   link at 60% of the transfer, restore after 90 seconds). Every profile has a
   fingerprint, so two runs are comparable or the report says they are not.
2. **Names what disappeared.** It diffs what the client believed it submitted
   against what the server actually holds, and classifies every difference. The
   finding that matters is not "an upload failed" — it is *the app reported
   success and the work is gone*.

The output is a loss report: per workflow, what was submitted, what arrived,
what vanished with no user-visible error, and what the server accepted in
truncated form.

## Status

The analysis, the profile definitions and the report generator are done and
tested. Container orchestration — starting an Android emulator, the proxy and a
headless-Chrome target, and applying the profile to their namespace — is not in
this repository yet; `plan` prints the exact commands that will do it, which is
also what lets a vendor's own network engineer audit the conditions before
accepting the findings.

## Install

```console
$ go install github.com/moveeeax/flaky-net-harness/cmd/flaky-net-harness@latest
```

Or from a clone:

```console
$ git clone https://github.com/moveeeax/flaky-net-harness.git
$ cd flaky-net-harness
$ go build -o bin/flaky-net-harness ./cmd/flaky-net-harness
```

Go 1.24 or newer. No dependencies outside the standard library.

## Usage example

Everything below runs against the fixtures in this repository, with no network,
no Docker and no accounts. The trace is one recorded run of an anonymised
field-service app under `nairobi-1700`.

### 1. See the conditions

```console
$ flaky-net-harness profiles
depot-basement  v1  fingerprint faa2e8fd3d56ea0c
  Usable link punctuated by a 40 second total stall, twice.
  link      768 kbit up / 3000 kbit down, 180 ms ±90 ms, 1% loss (25% correlated), queue 100 pkt
  budget    6m0s
  events    at t+45s         stall for 40s
            at t+200s        stall for 40s

motorway-handover  v1  fingerprint d8025cca96dcbd0f
  Hard disconnect at 60% of the transfer, link restored after 90 seconds.
  link      1500 kbit up / 6000 kbit down, 90 ms ±60 ms, 0.5% loss (20% correlated), queue 150 pkt
  budget    5m0s
  events    at  60% of transfer: disconnect for 90s

nairobi-1700  v1  fingerprint a9ca345d8411f9ef
  Sustained congestion on a shared HSPA cell at evening peak.
  link      320 kbit up / 1200 kbit down, 340 ms ±120 ms, 3.5% loss (40% correlated), queue 60 pkt
  budget    7m0s
  events    at  55% of transfer: kill_app
```

### 2. See exactly how they are applied

```console
$ flaky-net-harness plan --profile motorway-handover
# profile motorway-handover v1 (d8025cca96dcbd0f)
# interface eth0, ingress via ifb0, run budget 300s

### setup
...
# uplink: 1500 kbit, 90 ms delay ±60 ms, 0.5% loss
tc qdisc add dev eth0 root handle 1: netem limit 150 delay 90ms 60ms distribution normal loss 0.5% 20% reorder 0.3% rate 1500kbit
...

### timeline

# [upload=60%] disconnect
# Interface down mid-body. The server holds a partial object; the client holds a request it can no longer finish.
# hard teardown: the address goes away mid-transfer, as in a cell handover
ip link set dev eth0 down
sleep 90
# the radio comes back
ip link set dev eth0 up
```

Nothing is hidden: `plan` is the whole recipe, and `--interface`, `--ifb` and
`--app-container` adapt it to your topology.

### 3. Diff the client against the server

```console
$ flaky-net-harness analyze \
    --profile nairobi-1700 \
    --trace testdata/runs/nairobi-1700/client-trace.json \
    --server testdata/runs/nairobi-1700/server-state.json \
    --format md
```

Which opens with:

> # Loss report — Example FSM 4.2.1 (anonymised trial build)
>
> **Verdict: FAIL — 5 of 8 submitted items were destroyed or corrupted with no user-visible error**
>
> - **5 item(s) destroyed or corrupted with no user-visible error** (19.4 MB of operator work).
> - 1 delivered intact, 1 stored corrupted, 1 lost with an error shown, 1 stored twice.
> - 29.2 MB submitted, 24.6 MB never arrived.

and continues into the findings table:

| Severity | Object | Outcome | App said | Evidence |
|---|---|---|---|---|
| CRITICAL | `ph-003` | silent loss | nothing | no attempt completed (55% of the body sent, last status no response) and the app never told the operator |
| CRITICAL | `ph-004` | silent loss | nothing | no attempt completed (55% of the body sent, last status no response) and the app never told the operator |
| CRITICAL | `ph-006` | silent loss | nothing | no attempt completed (18% of the body sent, last status no response) and the app never told the operator |
| CRITICAL | `sig-0091` | silent loss | success | the app received HTTP 200 and showed no error, but the server has no such object |
| HIGH | `ph-002` | corrupted | nothing | server holds 1204224 bytes (29f0a11b99f5) under this id but the client sent 5100991 bytes (fecab750cd97): a partial body was accepted as complete |
| MEDIUM | `ph-001` | duplicated | success | stored 2 times under ids [ph-001 srv-att-77c1]: the retry path is not idempotent |
| MEDIUM | `ph-005` | reported loss | error shown | lost in transit (38% sent) and the app surfaced an error to the operator |
| INFO | `jc-4471` | delivered | success | arrived intact |

(the real table also carries the label, kind, workflow, size and attempt count
for each object — trimmed here for width)

For the artefact you actually hand over, write the self-contained HTML version:

```console
$ flaky-net-harness analyze \
    --profile nairobi-1700 \
    --trace testdata/runs/nairobi-1700/client-trace.json \
    --server testdata/runs/nairobi-1700/server-state.json \
    --format html --out out/report.html
FAIL — 5 of 8 submitted items were destroyed or corrupted with no user-visible error
wrote out/report.html (9 findings, 5 silent)
```

One page, no external assets, dark and light, with the reproduction script in
the appendix.

### 4. Use it as a gate

`analyze` exits **2** when work was destroyed or corrupted silently, **1** when
work was lost but the operator was told every time, and **0** when everything
arrived intact. `--fail-on silent|any|never` chooses which of those become a
non-zero process exit. The same fixtures show the clean case:

```console
$ flaky-net-harness analyze \
    --profile motorway-handover \
    --trace testdata/runs/motorway-handover/client-trace.json \
    --server testdata/runs/motorway-handover/server-state.json \
    --format md | head -3
# Loss report — Reference outbox app (control target shipped with the harness)

**Verdict: PASS — everything the client submitted is on the server, intact**
```

An app with a durable outbox passes the same class of run that destroys five
items in the other trace. The harness does not simply cry wolf.

## Why the classification is the product

| Outcome | Meaning | Severity |
|---|---|---|
| `delivered` | server holds it, hash matches | info |
| `silent_loss` | not on the server, operator never told | critical |
| `corrupted` | partial body accepted as complete | high (critical if the app also reported success) |
| `duplicated` | one payload stored under several ids | medium |
| `reported_loss` | lost, but the app surfaced an error | medium |
| `unattributed` | on the server, absent from the client trace | info |

`reported_loss` is deliberately not a headline finding. An app that loses an
upload on a dead link and says so is behaving correctly. The audit is about the
gap between what the operator was told and what the backend holds — which is why
`user_visible_error` is recorded per attempt, off the screen recording, and why
every object carries a SHA-256 so presence on the server is not mistaken for
delivery.

## Documentation

- [Network profiles](docs/profiles.md) — the three profiles, their real-world
  justification, the event kinds, and how to add one.
- [Trace and server-state format](docs/trace-format.md) — the two JSON inputs,
  field by field.
- [Audit scope](docs/audit-scope.md) — the fixed-price engagement this harness
  supports, and what it explicitly excludes.

## Not in scope

No SDK and no client library, on any platform. No hosted sync service, relay or
multi-tenant backend. No iOS. No dashboard, accounts or billing. No conflict
resolution, CRDTs or local database layer. No instrumentation of a vendor's
source code. The harness measures; the fixes belong to the vendor's engineers.

## Development

```console
$ go test ./...
$ go vet ./...
$ gofmt -l .
```

CI runs the same commands on every push, then re-runs the README example and
fails the build if it stops working.
