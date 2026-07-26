# Network profiles

Three profiles ship with the harness. Each one is a pinned set of `tc`/`netem`
parameters plus a timeline of adversarial events. Nothing is randomised: two
runs of the same profile version see the same conditions, which is what lets a
finding be re-run by the vendor's own engineer and get the same answer.

Every profile carries a **fingerprint** — a SHA-256 over its parameters and
events, truncated to 16 hex characters. It is printed in `profiles`, in `plan`
and in every report. If two reports quote different fingerprints, the runs are
not comparable, whatever the profile name says. Rewording a summary or a note
does not change the fingerprint; changing a number does.

```console
$ flaky-net-harness profiles
```

| Profile | Version | Fingerprint | Shape | Events |
|---|---|---|---|---|
| `nairobi-1700` | 1 | `a9ca345d8411f9ef` | 320 kbit up / 1200 kbit down, 340 ms ±120 ms, 3.5% loss (40% correlated), queue 60 pkt | `kill_app` at 55% of the transfer |
| `depot-basement` | 1 | `faa2e8fd3d56ea0c` | 768 kbit up / 3000 kbit down, 180 ms ±90 ms, 1% loss (25% correlated), queue 100 pkt | `stall` 40 s at t+45 s and t+200 s |
| `motorway-handover` | 1 | `d8025cca96dcbd0f` | 1500 kbit up / 6000 kbit down, 90 ms ±60 ms, 0.5% loss (20% correlated), queue 150 pkt | `disconnect` at 60% of the transfer, back after 90 s |

## nairobi-1700 — sustained congestion

An engineer syncing the day's job cards on the drive home shares one macro cell
with commuter traffic. Throughput collapses to a fraction of the advertised
rate, the round trip stretches past a third of a second, and loss is bursty
rather than uniform, so a single TCP flow spends most of its life in recovery.
This is the condition under which a 25 MB photo batch takes minutes instead of
seconds and the app's own request timeout starts firing.

The adversarial event is `kill_app` at 55% of the body: the engineer switches to
the maps app and Android reclaims the process mid-upload. No crash handler runs,
no dialog appears. If the app holds its outbox in memory, the work is gone and
the operator is never told.

## depot-basement — intermittent total stall

Underground parking and plant rooms give a link that looks connected — the radio
holds, the OS reports a network — while no packet completes a round trip. The
profile models this by blackholing traffic with `netem loss 100%` rather than
downing the interface, so sockets stay open and the client learns nothing for
40 seconds. Any code path that reads "no error yet" as "still fine" holds the
operator's work in memory until something discards it.

The stall fires twice: once early, once after the app has had time to believe
the sync succeeded.

## motorway-handover — hard disconnect mid-transfer

Driving between cells at speed produces a real interface teardown, not a slow
link: the address goes away mid-body and comes back ninety seconds later. This
is the profile that separates apps with a durable outbox from apps that hold the
job card in an in-memory queue and lose it the moment the socket dies. The
server is usually left holding a partial object, which is why the analysis
compares hashes and not just presence.

## Event kinds

| Kind | Effect | Reverts |
|---|---|---|
| `stall` | `tc qdisc change … netem loss 100%` on both directions; the interface stays up | after `for_seconds`, back to the steady-state shape |
| `disconnect` | `ip link set dev <iface> down` | after `for_seconds`: link up, then the shape is reapplied |
| `restore` | reapplies the steady-state shape | — |
| `kill_app` | `docker kill --signal=KILL <container>` | nothing to revert; the network is untouched |

An event fires either on the clock (`at_seconds`) or on the upload byte counter
(`at_transfer_pct`), never both. Transfer-triggered events cannot be scheduled
in advance — their wall-clock time depends on the target's own throughput — so
`plan` prints them last.

## Adding a profile

Profiles live in [`internal/profile/profiles.json`](../internal/profile/profiles.json)
and are embedded into the binary. They are validated at load time: a jitter
larger than the delay, an event past the run budget, an outage that is never
closed, or a loss figure outside 0–100 all fail the build's tests rather than
producing a report nobody can defend.

Changing a shipped profile's numbers must come with a version bump. The
fingerprint test in `internal/profile` fails loudly if you forget.
