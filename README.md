# submission-triage

Open-source Go service that an insurance agency runs against its inbox to
auto-check whether incoming submission emails contain all required documents,
reply (threaded) with what's missing, and escalate stalled cases. SQLite-backed,
single binary.

## What it does

- Watches an inbox: poll any IMAP mailbox (Gmail App Password, Microsoft 365,
  generic) or receive a Postmark inbound webhook.
- Parses email + attachments (PDF, DOCX, XLSX, CSV, plain).
- Classifies each attachment against a YAML checklist; consults the Anthropic
  API only when filename/keyword heuristics are inconclusive.
- Extracts structured fields via Anthropic tool-use when a checklist item
  declares `requires_field`.
- Replies in-thread with what's outstanding, or asks for clarification when the
  policy type can't be inferred.
- Tracks each case across follow-ups via Message-ID / In-Reply-To / References,
  not content matching.
- Escalates stale submissions, emails a periodic digest, and auto-closes
  completed ones after a quiet period.
- Writes a structured audit entry for every state change and external/LLM call.

## Mail channels

Inbound and outbound are independent; either can be Postmark or your own mailbox.

**IMAP + SMTP (five-minute on-ramp).** Point the tool at an existing mailbox —
no domain, DNS, or provider signup; a Gmail App Password works. The poller reads
unread mail every `IMAP_POLL_INTERVAL_SECONDS`, runs it through the pipeline,
replies over SMTP from the same mailbox, then marks it read.

```bash
IMAP_HOST=imap.gmail.com  IMAP_USERNAME=you@gmail.com  IMAP_PASSWORD=<app-password>
SMTP_HOST=smtp.gmail.com  SMTP_USERNAME=you@gmail.com  SMTP_PASSWORD=<app-password>
SMTP_FROM_ADDRESS=you@gmail.com
```

**Postmark (production).** Set `POSTMARK_SERVER_TOKEN` and
`POSTMARK_WEBHOOK_SECRET`; the `/webhooks/postmark` route mounts only when a
secret (or HMAC signature) is set, so there is no unauthenticated ingest.

Channel selection is by configuration: the poller starts when IMAP creds are
set, the webhook mounts when a secret is set (both can run at once), and
`OUTBOUND_PROVIDER` is `postmark` | `smtp` | `log` (empty = auto). Dedup is
channel-agnostic, so the same message arriving twice — even via different
channels — is processed once.

> Auth is password / App Password over TLS. OAuth2 (XOAUTH2) for Gmail / 365 is
> future work.

## Configuration

Configuration comes from environment variables. Copy `.env.example` to `.env`
and fill it in; the server loads it at startup. Defaults live in the struct
tags in [internal/config/config.go](internal/config/config.go).

## 30-second demo

```bash
git clone https://github.com/atdayev/submission-triage.git
cd submission-triage
make demo
```

`make demo` builds and runs the server, replays the `testdata/eml/` corpus
through the webhook, and tails the log. It needs a Unix-style shell (WSL, Git
Bash, or Linux/macOS); the other targets work on native Windows. No Postmark or
Anthropic key is required — without them the service uses a log-only sender and
the heuristic classifier, and wires the real ones in once you set the env vars.

## Architecture

Pragmatic clean architecture: lower layers know nothing about upper layers.

```
cmd/ ──> internal/app ──> internal/delivery ──> internal/service ──┬─> internal/repository
                                                                   └─> internal/infrastructure
internal/model    ── pure domain, no I/O
internal/database ── SQLite connection + migrations
pkg/ ── logger, retry, glob, postmarkeml, telemetry, ...
```

| Layer | Lives in | Owns |
|---|---|---|
| Domain | internal/model | entities, state machine, checklist evaluation |
| Repository | internal/repository | storage contracts + SQLite impl |
| Service | internal/service | use cases, idempotency, audit, retries |
| Delivery | internal/delivery | HTTP handlers, IMAP poller, payload→ingest mapping |
| Infrastructure | internal/infrastructure | Postmark/SMTP, Anthropic, doc extractors, checklist YAML |
| App | internal/app | composition root, graceful shutdown |

Cross-cutting guarantees: deterministic-ID idempotency (re-delivery is a no-op),
a structured audit log, exponential-backoff retries on external clients, a
bounded reply-worker pool off the request path, constant-time webhook auth,
OpenTelemetry metrics, and context-bounded graceful shutdown.

## Configurable checklists

One YAML file per policy type in `checklists/`. Drop in a new file and restart.

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

`requires_field` is enforced via Anthropic tool-use
([`ExtractField`](internal/infrastructure/llm/anthropic.go)); with no LLM
configured the field soft-passes on document presence.
`escalation.threshold_hours` overrides the global threshold, and
`action: email_digest` feeds the digest worker (`digest_recipient`, or the
global `ESCALATION_DIGEST_RECIPIENT`). Unknown keys are rejected at startup.
Five checklists ship: CGL, BOP, workers' comp, commercial property, cyber.

## Storage

SQLite via [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) (pure
Go, no CGO). The DB lives at `DB_PATH` (default `./data/submission-triage.db`);
migrations under [migrations/](migrations/) run at startup or via `make migrate`.

## Non-goals (v1)

AMS / CRM integration, web UI beyond `/health`, user accounts / multi-tenant
auth, SMS / Slack / Teams channels, producer dashboards.

## Coming next

AMS write-back (Applied EPIC, HawkSoft), read-only web UI, producer scorecards,
carrier-specific checklist overlays, Slack escalations, OAuth2 for Gmail / 365.

## License

MIT. See [LICENSE](LICENSE).

## Contact

Built by Atda Yev (atdayewkemal@gmail.com). PRs and issues welcome.
