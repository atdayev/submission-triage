# submission-triage

An open-source Go service that watches an insurance agency's submission inbox,
checks each incoming submission against a per-policy-type requirements
checklist, and replies in-thread with exactly what's missing. It runs against
any Gmail inbox with an App Password, stores everything in a single SQLite
file, and ships as one static binary.

## Why

Commercial submissions arrive incomplete. A broker emails over an ACORD
application but forgets the loss runs, or sends loss runs covering three years
when the carrier wants five. Today a human notices that — eventually — and
emails back to ask. Until they do, the file sits, the quote slips, and nobody
is sure whose turn it is.

submission-triage does that first pass automatically and immediately: it reads
the attachments, compares them to what the line of business actually requires,
and sends a clear "we still need X" reply on the same thread within seconds —
so the back-and-forth starts now instead of whenever someone gets to it.

## Status

Five lines of business ship today, each with its own checklist: **Commercial
General Liability, Business Owners Policy (BOP), Workers' Compensation,
Commercial Property, and Cyber Liability**.

## How it works

For each unread message in the watched mailbox:

1. **Poll** the inbox over IMAP (every `IMAP_POLL_INTERVAL_SECONDS`).
2. **Parse** the email and its attachments (PDF, DOCX, XLSX, CSV, plain text).
3. **Infer the policy type** from the subject line, then **classify** each
   attachment against that checklist — filename/keyword heuristics first,
   falling back to **Claude Haiku 4.5** only when the heuristics are
   inconclusive.
4. **Evaluate** the checklist: which required documents are present, and do
   declared fields meet their rule (e.g. loss runs covering ≥ 5 years).
5. **Reply in-thread** with what's outstanding — or ask which line of business
   it is when the subject doesn't say.
6. **Escalate** cases that go quiet, email a periodic digest, and auto-close
   completed ones after a quiet period.
7. **Audit** every state change and external call to SQLite.

Reliability: replies are written to a durable outbox in the same transaction
as the submission and redelivered until sent (surviving crashes and provider
outages), or dead-lettered after repeated failures. Ingest is idempotent on a
content hash over the Message-ID, body, and attachments, so the same message
seen twice is processed once.

## Quickstart (~15 minutes)

You need Go 1.25+ and a Gmail account.

**1. Clone and build.**

```bash
git clone https://github.com/atdayev/submission-triage.git
cd submission-triage
make build          # produces ./bin/server
```

**2. Get a Gmail App Password.** Enable 2-Step Verification on the account,
then create a 16-character App Password at
<https://myaccount.google.com/apppasswords>. This lets the service log in over
IMAP/SMTP without your real password.

**3. Configure.** Copy the example env file and fill in the IMAP and SMTP
blocks (for Gmail they're the same account and the same App Password):

```bash
cp .env.example .env
```

```bash
IMAP_HOST=imap.gmail.com   IMAP_USERNAME=you@gmail.com   IMAP_PASSWORD=<app-password>
SMTP_HOST=smtp.gmail.com   SMTP_USERNAME=you@gmail.com   SMTP_PASSWORD=<app-password>
SMTP_FROM_ADDRESS=you@gmail.com
```

**4. (Optional) Enable the LLM.** Set `ANTHROPIC_API_KEY` to let Claude resolve
attachments the heuristics can't and read declared checklist fields. Without a
key the service still runs; ambiguous classification falls back to the
heuristic and field rules pass on document presence.

**5. Start it.**

```bash
make run            # build + run; or ./bin/server after make build
```

**6. Send a test.** From another account, email the watched inbox with a
subject naming the line of business (e.g. `New CGL submission – Acme LLC`) and
attach an ACORD 125. Within the poll interval (default 30s) plus normal mail
delivery, you'll get a threaded reply listing whatever the CGL checklist still
wants. Reply with the missing documents and it continues the thread; when the
file is complete it sends a "moving to underwriting" note.

**7. Run it in production.** Keep it always-on under a process manager — it's a
long-lived IMAP poller and escalation worker, not a request-driven service. A
ready systemd unit (install steps in its header) lives at
[`deploy/systemd/submission-triage.service`](deploy/systemd/submission-triage.service).

## Configuration

All configuration is environment variables, loaded from `.env` at startup.
`.env.example` documents every one; the essentials:

**IMAP — the inbox to watch (required).** The poller is inactive until host,
username, and password are all set.

| Variable | Purpose | Default |
|---|---|---|
| `IMAP_HOST` | IMAP server (`imap.gmail.com` for Gmail) | — (required) |
| `IMAP_USERNAME` | Mailbox login | — (required) |
| `IMAP_PASSWORD` | App Password | — (required) |
| `IMAP_PORT` | IMAP port (implicit TLS) | `993` |
| `IMAP_MAILBOX` | Folder to watch | `INBOX` |
| `IMAP_POLL_INTERVAL_SECONDS` | How often to check for mail | `30` |
| `IMAP_MAX_MESSAGE_MB` | Skip messages larger than this (logged once, marked read) | `32` |

**SMTP — where replies are sent from (required).** Same mailbox/App Password as
IMAP for Gmail. Port 587 uses STARTTLS, 465 uses implicit TLS.

| Variable | Purpose | Default |
|---|---|---|
| `SMTP_HOST` | SMTP server (`smtp.gmail.com` for Gmail) | — (required) |
| `SMTP_USERNAME` | Mailbox login | — (required) |
| `SMTP_PASSWORD` | App Password | — (required) |
| `SMTP_FROM_ADDRESS` | From address on replies | — (required) |
| `SMTP_PORT` | SMTP port | `587` |
| `SMTP_FROM_NAME` | Display name on replies | `Submission Triage` |
| `OUTBOUND_PROVIDER` | `smtp`, `log`, or blank (auto) | auto |

**LLM — Anthropic (optional).**

| Variable | Purpose | Default |
|---|---|---|
| `ANTHROPIC_API_KEY` | Enables LLM classification + field extraction | — (off if blank) |
| `ANTHROPIC_MODEL` | Model id | `claude-haiku-4-5` |
| `ANTHROPIC_TIMEOUT_SECONDS` | Per-call timeout | `30` |
| `ANTHROPIC_MAX_TOKENS` | Output token cap | `2048` |

**Escalation timers (optional).**

| Variable | Purpose | Default |
|---|---|---|
| `ESCALATION_INTERVAL_MINUTES` | How often the worker runs | `15` |
| `ESCALATION_THRESHOLD_HOURS` | Quiet time before a case escalates | `72` |
| `ESCALATION_AUTO_CLOSE_AFTER_HOURS` | Auto-close after quiet hours | `336` |
| `ESCALATION_DIGEST_INTERVAL_HOURS` | Digest send cadence | `24` |
| `ESCALATION_DIGEST_RECIPIENT` | Digest recipient | — (off if blank) |

**Other (optional):** `DB_PATH` (`./data/submission-triage.db`),
`CHECKLISTS_DIR` (`./checklists`), `HTTP_PORT` (`8080`, health endpoint only),
logging (`LOG_LEVEL`, `LOG_FORMAT`, `LOG_DIR`), reply worker pool
(`REPLY_WORKERS`, `REPLY_QUEUE_SIZE`), and retries (`RETRY_ATTEMPTS`,
`RETRY_BASE_DELAY_MS`). See `.env.example` for all of them.

## Customizing checklists

A checklist is one YAML file per policy type in `checklists/`. Add a file and
restart. Here's an excerpt of the shipped CGL checklist, annotated (the real
file lists more required items):

```yaml
name: Commercial General Liability
policy_type: cgl                 # matched against the inferred line of business
required_items:
  - id: acord_125
    description: "ACORD 125 Commercial Insurance Application"   # shown to the sender
    match:
      filename_patterns: ["*ACORD*125*", "*application*"]       # tried first
      content_keywords: ["Commercial Insurance Application"]    # then document text
  - id: loss_runs
    description: "Loss runs for the past 5 years"
    match:
      filename_patterns: ["*loss*run*", "*claims*history*"]
      content_keywords: ["Loss Run", "Claims History"]
    requires_field:            # the only supported predicate beyond presence
      name: years_covered      # field the LLM extracts from the document
      type: number
      min_value: 5             # fails if the value is below this
      unit: years              # noun used in the customer-facing reply
escalation:
  threshold_hours: 72          # overrides the global threshold for this line
```

`requires_field` is the **only** rule beyond "is the document present", and it
needs the LLM enabled to extract the value (without a key it passes on
presence). Wording like "5 years" in the other four checklists is descriptive
text only — it is not enforced. Unknown YAML keys are rejected at startup.

## Help

Questions, bugs, or "would this work for my agency?" — open a
[GitHub issue](https://github.com/atdayev/submission-triage/issues) or DM the
maintainer on LinkedIn.

## Contributing

Issues and pull requests are welcome — for anything non-trivial, open an issue
first to discuss the approach. CI runs gofmt, golangci-lint, and `go test -race`
on every PR; keep it green.

## License

MIT. See [LICENSE](LICENSE).
