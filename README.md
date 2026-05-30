# submission-triage

Open-source Go service that an insurance agency runs against its inbox to
auto-check whether incoming submission emails contain all required
documents, reply (threaded) with what's missing, and escalate stalled
cases. SQLite-backed, single binary.

---

## Why this exists

Half of every new-business submission email is missing something — the
ACORD 126, the loss runs, the schedule of locations. A CSR catches it hours
later, lists what's needed, and waits. The producer sends one of the three
items a day later. A few rounds and several days in, the carrier has already
quoted three competitors.

This service does the dull part of that loop without taking it over: it
reads the inbox, checks each submission against a per-policy-type checklist,
and chases the gaps in-thread.

It is intentionally narrow. There is no AMS integration, no portal, no UI
beyond `/health`. Those are appropriate as v2 features once the core
loop is proving itself.

## What it does

- Watches an inbox — poll any IMAP mailbox (Gmail App Password, Microsoft
  365, generic) or receive a Postmark inbound webhook.
- Parses email + attachments (PDF, DOCX, XLSX, CSV, plain).
- Classifies each attachment against a YAML-defined checklist.
- Extracts structured fields from attachments via Anthropic tool-use
  when a checklist item declares `requires_field`.
- Sends a threaded reply listing what's still outstanding (or asks for
  clarification when the policy type can't be inferred).
- Tracks each case across follow-up emails via Message-ID / In-Reply-To /
  References — no LLM matching for thread identity.
- Escalates submissions that go stale past a configurable threshold and
  emails a periodic digest of escalations to a configured recipient.
- Auto-closes Complete submissions after a configurable quiet period.
- Writes a structured audit entry for every state change, every external
  call, and every LLM call (prompt hash, latency, token usage, estimated
  cost in USD).

## Mail channels

Inbound and outbound are independent and either can be Postmark or your own
mailbox. Pick the on-ramp that fits.

**IMAP + SMTP (five-minute on-ramp).** Point the tool at an existing mailbox.
No domain, no DNS, no provider signup — a Gmail App Password is enough. The
poller checks the inbox every `IMAP_POLL_INTERVAL_SECONDS`, runs each new
unread message through the pipeline, replies over SMTP from the same mailbox
(threaded via `Message-ID` / `In-Reply-To` / `References`), then marks the
message read.

```bash
IMAP_HOST=imap.gmail.com  IMAP_USERNAME=you@gmail.com  IMAP_PASSWORD=<app-password>
SMTP_HOST=smtp.gmail.com  SMTP_USERNAME=you@gmail.com  SMTP_PASSWORD=<app-password>
SMTP_FROM_ADDRESS=you@gmail.com
```

**Postmark (production).** Set `POSTMARK_SERVER_TOKEN` and a
`POSTMARK_WEBHOOK_SECRET`; the `/webhooks/postmark` route registers itself and
ingests Postmark's inbound payloads. The webhook route is mounted **only** when
a secret (or HMAC signature secret) is set — there is no unauthenticated public
ingest endpoint.

**Channel selection** is by configuration at startup:

- Inbound: the IMAP poller starts when `IMAP_HOST`/`USERNAME`/`PASSWORD` are
  set; the webhook mounts when a webhook secret is set. Both can run at once.
- Outbound: `OUTBOUND_PROVIDER` = `postmark` | `smtp` | `log`, or empty for
  auto (SMTP if configured, else Postmark if a token is set, else log-only).

Idempotency is channel-agnostic — the deterministic email ID is
`SHA256(Message-ID + body + sorted attachment hashes)`, so the same message
arriving via both channels dedupes. Every audit entry records the channel it
used (`source: postmark|imap` on inbound, `via: postmark|smtp|log` on
outbound) so actions stay unambiguous when both modes run together.

> Auth today is password / App Password (IMAP/SMTP over TLS). OAuth2 (XOAUTH2)
> for Gmail / 365 is future work.

## 30-second demo

```bash
git clone https://github.com/atdayev/submission-triage.git
cd submission-triage
make demo
```

`make demo` builds the server, runs it with SQLite in `./data/`, replays
the `testdata/eml/` corpus through the inbound webhook, and tails the
server log so you can watch the state machine fill in. Requires a
Unix-style shell (`tail -f`, `kill`, background-process job control) —
use WSL, Git Bash, or a Linux/macOS host. On native Windows the build,
test, and run targets all work; only `make demo` is shell-specific.

You do not need a Postmark or Anthropic key for the demo; the service
falls back to a log-only mail sender and the heuristic classifier when
those are absent. Both are wired the moment you set the env vars.

## Architecture

Clean architecture, pragmatic Go variant. Strict layer rules; lower
layers know nothing about upper layers.

```
cmd/ ──> internal/app ──> internal/delivery ──> internal/service ──┬─> internal/repository
                                                                   │
                                                                   └─> internal/infrastructure
internal/model    ── pure domain, no I/O, no clock, no logger
internal/database ── SQLite connection + migrations runner
internal/repository/mocks ── generated, used by tests only
pkg/ ── logger, apperror, utils, retry, glob, postmarkeml, telemetry
```

| Layer | Lives in | Owns | May import |
|---|---|---|---|
| Domain | [internal/model/](internal/model/) | entities, state machine, pure checklist evaluation | stdlib only |
| Repository | [internal/repository/](internal/repository/) | storage contracts + SQLite impl | model, database |
| Service | [internal/service/](internal/service/) | use cases, idempotency, audit, retries | repository (interfaces), infrastructure (interfaces) |
| Delivery | [internal/delivery/](internal/delivery/) | HTTP handlers, IMAP poller, shared payload→ingest mapping | service, pkg/* |
| Infrastructure | [internal/infrastructure/](internal/infrastructure/) | adapters for Postmark, Anthropic, PDF, DOCX, XLSX, CSV, checklist YAML | model, pkg/* |
| App | [internal/app/app.go](internal/app/app.go) | composition root, graceful shutdown | everything |
| Cmd | [cmd/server/main.go](cmd/server/main.go) | entry point | app only |

If a lower layer needs to import from a higher layer, the dependency is
pointing the wrong way. Stop and restructure.

## Production patterns

**Idempotency.** Every inbound email is hashed into a deterministic ID
(`SHA256(Message-ID + body + sorted attachment hashes)`). Repository
writes are upsert-by-ID. Re-delivery is a no-op. See
[`computeEmailID`](internal/service/submissions.go) and
[`upsertEmailRow`](internal/repository/submissions.go).

**Audit log.** Every state change, every LLM call, every email send, and
every external error gets a structured `AuditEntry` row. Queryable by
submission and event type. See
[internal/model/audit.go](internal/model/audit.go) and
[internal/repository/audit.go](internal/repository/audit.go).

**Retries with exponential backoff.** All external clients (LLM, mail)
go through [`pkg/retry.Do`](pkg/retry/retry.go). Each retry logs at Warn
level. Permanent errors short-circuit; transient ones back off.

**Threaded reply handling.** Cases link across emails via Message-ID /
In-Reply-To / References, never content matching. Multiple matches log
an audit warning, never silent merging. See
[`FindByEmailReference`](internal/repository/submissions.go).

**LLM as tool, not orchestrator.** The classifier tries filename glob
matching first, then content-keyword matching. The LLM is consulted only
when heuristics produce zero or multiple candidates. See
[internal/infrastructure/classifier/classifier.go](internal/infrastructure/classifier/classifier.go).
LLM failures don't block the pipeline; the audit log records the failure
and the escalation worker retries on its own schedule.

**Concurrent ingest collapse.** Two webhook deliveries for the same
email arriving in parallel goroutines used to race past the
duplicate-check window and create orphan submissions. A
`singleflight.Group` keyed on the deterministic email ID now collapses
concurrent ingests of the same email into a single execution — every
caller gets the same `IngestResult`. See `ingestEmailInner` in
[internal/service/submissions.go](internal/service/submissions.go).

**Bounded reply sending.** Threaded replies are sent off the request path
by a fixed worker pool over a buffered channel, so a slow or down Postmark
can't spawn unbounded goroutines. A full queue drops and audits rather than
blocking the webhook (which would just trigger Postmark redelivery). See
[internal/service/submissions.go](internal/service/submissions.go).

**Webhook authentication.** The inbound webhook accepts a constant-time
shared-secret header or an HMAC-SHA256 body signature (either, or both).
See [internal/delivery/http/handler/webhook.go](internal/delivery/http/handler/webhook.go).

**Metrics.** OpenTelemetry counters and histograms for ingest, LLM calls,
replies, and escalations, exported via OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT`
is set and a no-op otherwise. See [pkg/telemetry](pkg/telemetry/telemetry.go).

**Graceful shutdown.** SIGINT / SIGTERM cancels the root context, the
HTTP server is given `shutdown_timeout_seconds` to drain, the escalation
worker is joined via WaitGroup, the in-flight reply pool is drained, then
the DB handle closes. See [internal/app/app.go](internal/app/app.go).

**Request ID propagation.** Middleware generates a request ID, attaches
it to the per-request logger entry, and includes it in every audit
entry. See [internal/delivery/http/middleware.go](internal/delivery/http/middleware.go).

## Configurable checklists

One YAML file per policy type in `checklists/`. Drop in a new file and
restart to add a new policy type.

```yaml
name: Commercial General Liability
policy_type: cgl
required_items:
  - id: acord_125
    description: "ACORD 125 Commercial Insurance Application"
    match:
      filename_patterns: ["*ACORD*125*", "*application*"]
      content_keywords: ["Commercial Insurance Application"]
  - id: loss_runs
    description: "Loss runs for the past 5 years"
    match:
      filename_patterns: ["*loss*run*", "*claims*history*"]
      content_keywords: ["Loss Run", "Claims History"]
    requires_field:
      name: years_covered
      type: number
      min_value: 5
escalation:
  threshold_hours: 72
  action: email_digest
  digest_recipient: ops@agency.example
```

`requires_field` is enforced via the LLM's tool-use API
([`ExtractField`](internal/infrastructure/llm/anthropic.go)) — the service
calls Anthropic with a typed JSON schema, the model returns the extracted
value, and the checklist evaluator validates against `min_value`. When no
LLM is configured, the system soft-passes the field and accepts document
presence as the satisfaction signal.

`escalation.threshold_hours` overrides the global escalation threshold for
that policy type. `escalation.action: email_digest` participates in the
periodic digest worker, which emails `digest_recipient` (or the global
`escalation.digest_recipient` in `config.yaml`) on the configured interval.
The checklist YAML is loaded strictly: unknown keys are rejected at
startup.

Five checklists ship: `commercial_general_liability`, `business_owners_policy`,
`workers_compensation`, `commercial_property`, `cyber_liability`.

## Storage

SQLite via [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) —
pure Go, no CGO, no system libraries. The DB file lives at `DB_PATH` (default
`./data/submission-triage.db`). Migrations live under [migrations/](migrations/)
and run automatically at startup, or on demand via `make migrate`.

## What's NOT included

These are explicit non-goals for v1 — not "we forgot."

- AMS / CRM integration (Applied, EZLynx, HawkSoft)
- Web UI beyond `/health`
- User accounts / multi-tenant auth (single-tenant assumed — the inbound
  webhook itself takes a shared secret or HMAC signature)
- SMS, Slack, Teams channels
- Producer accountability dashboard
- Multi-tenant
- MCP delivery

## Coming next

- AMS write-back via Applied EPIC and HawkSoft APIs
- Read-only web UI for case lists and audit history
- Producer scorecard rollups (missing-doc rates by producer)
- Carrier-specific checklist overlays
- Slack escalation channel as an alternative to email digests
- OAuth2 (XOAUTH2) for Gmail / Microsoft 365 inbound and outbound

## License

MIT. See [LICENSE](LICENSE).

## Contact

Built by Atda Yev (atdayewkemal@gmail.com). PRs and issues welcome.
