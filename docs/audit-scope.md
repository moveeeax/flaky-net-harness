# Flaky-network data-loss audit — fixed scope

A field-service app that loses a job card on a congested cell does not usually
crash. It shows a tick, the engineer drives to the next call, and the office
finds out three days later. This audit establishes, on video and against the
vendor's own backend, whether that happens in their app — and what to change.

**Price: EUR 2,000, fixed. Two weeks from the kick-off call.**

## What you get

1. **Three runs per workflow** against the three published network profiles
   (`nairobi-1700`, `depot-basement`, `motorway-handover`), each pinned to
   concrete bandwidth, latency, loss and jitter figures and identified by a
   fingerprint, so any result can be re-run by your own engineers.
2. **A screen recording per run**, showing what the operator saw at the moment
   the work was destroyed.
3. **A loss report** naming, per workflow, every item submitted, every item that
   arrived, and every item that disappeared with no user-visible error —
   including partial objects your backend accepted as complete.
4. **A fix plan**: the specific changes, in priority order, that would have
   prevented each finding, sized in engineering days by your team's own
   architecture. Durable outbox, idempotency keys, resumable upload, honest
   failure surfaces.
5. **A walkthrough call** with your mobile and backend engineers, and the raw
   traces so they can reproduce every finding themselves.

## What you need to provide

- A build of the app (a store build is fine) or a trial account.
- A test tenant on your backend, plus a way to list what it holds after a run —
  an export endpoint, an admin screen, or a database query your engineer runs.
- One named engineer for questions during the two weeks.

## Explicitly not included

- No SDK, library or patch. This audit reports and recommends; it does not ship
  code into your app.
- No hosted upload service, relay or backend of any kind.
- No iOS. Android and web/PWA only.
- No source-code review, no security assessment, no penetration testing.
- No load or capacity testing. This is about correctness under a bad link, not
  throughput.
- No dashboard, no accounts, no ongoing monitoring. A CI mode that re-runs the
  harness against each release exists as a separate engagement, and only after
  an audit has been delivered.
- No fix implementation, and no re-test retainer, unless quoted separately.

## How it runs

| Day | |
|---|---|
| 0 | Kick-off call: agree the three to five workflows that matter, get the build and the test tenant. |
| 1–5 | Runs. Every workflow against every profile, recorded. |
| 6–8 | Correlation against your backend's state, report drafted. |
| 9–10 | Fix plan sized, walkthrough call, artefacts handed over. |

## Fair-dealing terms

- If the audit finds no silent data loss across all three profiles, you get the
  report, the recordings and the raw traces, and pay half. A clean result is a
  finding worth publishing internally.
- Findings are yours. Nothing is published, named or shown to anyone else
  without written permission.
- The harness and the profiles are public. The conditions are not a trade
  secret; the point is that you can re-run them.

Contact: open an issue on this repository, or use the address on the repository
owner's GitHub profile.
