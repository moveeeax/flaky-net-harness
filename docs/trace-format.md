# Trace and server-state format

`analyze` takes two JSON files. Both are plain, small and human-readable on
purpose: during an audit the server side is usually produced by the vendor's own
engineer, and they must be able to write it in ten minutes and check it by eye.

Unknown fields are rejected. A typo in a field name would otherwise be dropped
silently and change how a finding is classified.

## Client trace

What the client believed it submitted, as observed on the wire by the harness
proxy. See [`testdata/runs/nairobi-1700/client-trace.json`](../testdata/runs/nairobi-1700/client-trace.json).

```jsonc
{
  "run": {
    "target": "Example FSM 4.2.1 (anonymised trial build)",
    "target_kind": "android-emulator",      // or web-pwa
    "profile": "nairobi-1700",
    "profile_version": 1,
    "profile_fingerprint": "a9ca345d8411f9ef",
    "started_at": "2026-07-18T17:04:11Z",
    "operator": "harness",
    "recording": "out/nairobi-1700/screen.mp4"
  },
  "attempts": [
    {
      "id": "a2",                            // unique within the trace
      "workflow": "photo_batch_upload",      // the operator-visible task
      "method": "POST",
      "url": "https://api.example-fsm.invalid/v2/jobs/4471/photos",
      "started_at_ms": 11200,                // ms since the start of the run
      "ended_at_ms": 214600,
      "declared_bytes": 26214400,            // request Content-Length
      "sent_bytes": 14418944,                // what actually left the client
      "status_code": 0,                      // 0 = no response line ever arrived
      "transport_error": "connection reset by peer",
      "user_visible_error": false,           // did the app tell the operator?
      "objects": [
        {
          "id": "ph-003",
          "kind": "photo",                   // job_card | photo | signature | …
          "label": "Fault — cracked heat exchanger",
          "bytes": 5233118,
          "sha256": "e46b34ae…"
        }
      ]
    }
  ]
}
```

Two fields carry most of the weight:

- **`user_visible_error`** is read off the screen recording, not off the wire.
  It is the difference between "this app is slow on a bad network" and "this app
  destroys work and lies about it". Record it honestly; the report says so when
  no recording is referenced.
- **`sha256`** per object lets a truncated body that the server accepted be told
  apart from a complete one. Presence alone is not evidence of delivery.

The same object may appear in several attempts — that is a retry — but it must
carry the same hash each time.

## Server state

Everything the backend holds for this job after the run. See
[`testdata/runs/nairobi-1700/server-state.json`](../testdata/runs/nairobi-1700/server-state.json).

```jsonc
{
  "source": "vendor export: GET /v2/jobs/4471/attachments, taken 20 minutes after the run",
  "objects": [
    { "id": "ph-001", "kind": "photo", "bytes": 4812003, "sha256": "d7be88bc…", "received_at_ms": 96200 }
  ]
}
```

`source` is quoted verbatim in the report footer. Say where the export came from
and when it was taken — a state captured before the app's background retry ran
would understate the loss and the vendor will, rightly, say so.

An object the server holds under an id the client never used shows up as
`unattributed`. If it shares a hash with a delivered object, the delivered
object is reclassified as `duplicated`: the retry path is not idempotent.

## Outcomes

| Outcome | Meaning | Severity |
|---|---|---|
| `delivered` | server holds it, hash matches | info |
| `silent_loss` | not on the server, and the operator was never told | critical |
| `corrupted` | server holds a different payload under the same id — a partial body accepted as complete | high, critical if the app also reported success |
| `duplicated` | one payload stored under several ids | medium |
| `reported_loss` | not on the server, but the app surfaced an error | medium |
| `unattributed` | on the server, absent from the client trace | info |

Exit codes: `2` when the run destroyed or corrupted work silently, `1` when work
was lost but the operator was told every time, `0` when everything arrived
intact. `--fail-on any|silent|never` chooses which of those become a non-zero
process exit, which is what makes the same analysis usable as a CI gate later.
