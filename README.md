# submission-triage

An open-source service that watches an insurance agency's submission inbox,
checks every incoming submission against a checklist for that line of business,
and replies in-thread with exactly what's missing.

It runs against a normal Gmail mailbox, stores everything in a single file, and
ships as one binary with nothing else to install.

## Why

Commercial submissions arrive incomplete. A broker emails an ACORD application
but forgets the loss runs, or sends loss runs covering three years when the
carrier wants five. Today a human notices that — eventually — and emails back to
ask. Until they do, the file sits, the quote slips, and nobody is sure whose
turn it is.

submission-triage does that first pass immediately: it reads the attachments,
compares them to what the line of business actually requires, and sends a clear
"we still need X" reply on the same thread within seconds. The back-and-forth
starts now instead of whenever someone gets to it.

## What a broker sees

When something is missing, they get a reply on their own thread:

```text
Subject: Re: New CGL submission - Oakview Construction

Hi Diane,

Thanks for the submission. To finish the file we still need:

  - Loss runs for the past 5 years
  - Schedule of insured locations

Reply to this thread with the documents and we'll continue.
```

When they send the rest, the thread closes out:

```text
Hi Diane,

Thanks — everything on our checklist for this submission is now accounted for.
Nothing further is needed from you at this time.
```

Two variations show up as needed: asking which line of business it is when the
subject doesn't say, and asking for a file to be resent when it arrived
password-protected or as a download link instead of an attachment. If a document
arrives but falls short of a rule, the bullet says so — *"Loss runs for the past
5 years (covers only 3 years, need at least 5)"*.

Replies are plain text with no footer or disclaimer, go to the sender's Reply-To
address if they set one, never copy the original CC list, and are marked
auto-generated so other autoresponders stay quiet.

The service never speaks for the agency. It reports what it can and can't see,
and makes no promise about what happens next — no "moving to underwriting", no
"you'll hear back shortly". That's enforced by a test, not just convention.

## What the agency sees

**The mailbox organizes itself.** Each message is filed into a folder as it's
processed:

| Folder | What's in it |
|---|---|
| `Triage/Ready for Underwriting` | Everything on the checklist is accounted for |
| `Triage/Waiting on Broker` | A reply went out; waiting on documents |
| `Triage/Escalated` | The broker went quiet, or the bind date is close |
| `Triage/Unknown Policy` | The line of business couldn't be determined |
| `Triage/Hold` | Paused by a person — see below |

Anything a human should look at is **starred** — usually a document that arrived
as an unreadable scan, or one the service couldn't confidently identify. Those
are never mentioned to the broker; they're the agency's to resolve.

**Drag a message into `Triage/Hold` to pause it.** No replies, no escalation, no
auto-close, for as long as it sits there. Drag it out to resume. That's the whole
gesture — the folder *is* the switch.

**A daily digest** lands in the agency's inbox with every open submission
grouped by what needs attention. It's skipped entirely on a day with nothing
open, so an empty one never trains people to ignore it:

```
1 submission(s) need review (marked below).

Open submissions:

Escalated — broker went quiet (1):
  - Oakview Construction | from diane@acme-broker.com | age 5d | idle 4d | binds in 3d
      Loss runs for the past 5 years: document not provided

Awaiting the broker (1):
  - Riverbend Diner | from sam@broker.example | age 2h | idle 1h [needs review]
      Schedule of insured locations: document not provided
```

## Lines of business

Five ship today, each with its own checklist you can edit:

**Commercial General Liability**, **Business Owners Policy**, **Workers
Compensation**, **Commercial Property**, and **Cyber Liability**.

## How it works

Every 30 seconds it checks the mailbox for unread mail. For each message it:

1. Reads the email and its attachments — PDF, DOCX, XLSX, CSV, plain text.
2. Works out the line of business from the subject, or from the broker's reply
   if the subject didn't say.
3. Identifies each attachment against that checklist, by filename and document
   text first, asking Claude only when that's inconclusive.
4. Checks what's still outstanding — including rules like "loss runs covering at
   least 5 years", not just "a file called loss runs is present".
5. Replies in-thread, files the message into its folder, and stars it if a human
   should look.

Then, on its own schedule: it nudges submissions that go quiet, escalates ones
whose bind date is approaching, sends the daily digest, and closes out
submissions that have been settled for a while.

If it's mid-reply when the machine restarts, the reply still goes out. If the
same message arrives twice, it's handled once — and a broker never gets the same
request twice in a row.

## Install

You need **Go 1.25+** and a Gmail account. Nothing else — no database server, no
container runtime.

**1. Build.**

```bash
git clone https://github.com/atdayev/submission-triage.git
cd submission-triage
make build          # produces ./bin/server
```

**2. Get a Gmail App Password.** Turn on 2-Step Verification for the account,
then create a 16-character App Password at
<https://myaccount.google.com/apppasswords>. This lets the service sign in
without your real password, and you can revoke it independently.

**3. Configure.** Copy the example file and fill in the mailbox:

```bash
cp .env.example .env
```

```bash
IMAP_HOST=imap.gmail.com   IMAP_USERNAME=you@gmail.com   IMAP_PASSWORD=<app-password>
SMTP_HOST=smtp.gmail.com   SMTP_USERNAME=you@gmail.com   SMTP_PASSWORD=<app-password>
SMTP_FROM_ADDRESS=you@gmail.com
DIGEST_RECIPIENT=underwriting@youragency.com
```

Optionally set `ANTHROPIC_API_KEY` to let Claude identify attachments the
filename and keyword rules can't place, and read values like "years covered" out
of a document. Without a key it still runs — identification falls back to
filename and keyword matching, and rules like the 5-year one pass as long as the
document is there.

**⚠️ If the mailbox already has mail in it, set `IMAP_IGNORE_BEFORE` too** (see
[First week](#first-week)). Otherwise the first run treats every old unread
email as a new submission and replies to all of them.

**4. Run it.**

```bash
make run
```

**5. Send a test.** From another account, email the watched inbox with a subject
naming the line of business — `New CGL submission - Acme LLC` — and attach an
ACORD 125. Within a minute you'll get a threaded reply listing what the CGL
checklist still wants. Reply with those documents and the thread completes.

### Running it for real

It's a long-lived mailbox watcher, not a request-driven web service, so it needs
to stay running. A ready systemd unit with copy-paste install steps in its header
is at [`deploy/systemd/submission-triage.service`](deploy/systemd/submission-triage.service):

```bash
sudo cp bin/server /opt/submission-triage/submission-triage
sudo cp -r checklists migrations /opt/submission-triage/
sudo cp .env /etc/submission-triage/env
sudo chmod 600 /etc/submission-triage/env      # it holds the mailbox password
sudo systemctl enable --now submission-triage
```

Logs go to `journalctl -u submission-triage -f`. `GET /health` on port 8080
reports whether the mailbox is still reachable — point a monitor at it.

If you deploy to a Google Cloud VM, `scripts/deploy.sh` cross-compiles and ships
a new binary over your own `gcloud` credentials; configure it via
`.deploy.env.example`.

## First week

Three settings exist specifically so you can point this at a real mailbox
without it doing anything you didn't intend.

| Setting | Use it to |
|---|---|
| `IMAP_IGNORE_BEFORE` | Ignore mail older than a date (RFC3339, e.g. `2026-01-01T00:00:00Z`). **Set this on any mailbox with history** — without it the first run replies to every old unread email. |
| `REPLIES_ENABLED=false` | Run everything — reading, identifying, filing, starring, the digest — but send brokers nothing. Turn it on when the digest looks right. |
| `Triage/Hold` | Pause one submission by hand, any time (drag the message into the folder). |

Nothing queued while `REPLIES_ENABLED=false` is sent later when you turn replies
back on. Those submissions appear under their own heading in the digest so you
can pick them up deliberately.

## What it costs

**Claude usage** is a few tenths of a cent per submission, and only for
attachments the filename and keyword rules can't place. `LLM_DAILY_USD_CAP`
(default `$10.00`) is a hard ceiling per day — past it, identification falls back
to filename matching for the rest of the day rather than spending more.

**Disk** is roughly 60 KB per submission, so a busy agency at 200 submissions a
day uses a few GB a year. Old records are pruned automatically after
`AUDIT_RETENTION_DAYS` (default 90).

## Configuration

Everything is environment variables, read from `.env` at startup.
**[`.env.example`](.env.example) documents every one**, grouped and commented.
The ones most agencies actually change:

| Variable | What it does | Default |
|---|---|---|
| `IMAP_HOST` `IMAP_USERNAME` `IMAP_PASSWORD` | The mailbox to watch | — required |
| `SMTP_HOST` `SMTP_USERNAME` `SMTP_PASSWORD` `SMTP_FROM_ADDRESS` | Where replies come from | — required |
| `DIGEST_RECIPIENT` | Who gets the daily digest. Blank means no digest — the folders still organize, but nobody is told anything | — off if blank |
| `IMAP_IGNORE_BEFORE` | Ignore mail older than this date | first run |
| `REPLIES_ENABLED` | `false` to run without emailing brokers | `true` |
| `ANTHROPIC_API_KEY` | Enables Claude-assisted identification | — off if blank |
| `LLM_DAILY_USD_CAP` | Daily spend ceiling | `10.00` |
| `IMAP_POLL_INTERVAL_SECONDS` | How often to check for mail | `30` |
| `ESCALATION_THRESHOLD_HOURS` | Quiet time before a submission is escalated | `72` |
| `DIGEST_INTERVAL_HOURS` | How often the digest goes out | `24` |
| `IMAP_MAILBOX` | Which folder to watch | `INBOX` |
| `IMAP_FOLDER_PREFIX` | Prefix for the status folders | `Triage` |

The rest are timing and safety limits with sensible defaults; you shouldn't need
to touch them.

## Customizing checklists

One YAML file per line of business in `checklists/`. Add or edit a file and
restart — here's the shipped CGL checklist, trimmed:

```yaml
name: Commercial General Liability
policy_type: cgl
required_items:
  - id: acord_125
    description: "ACORD 125 Commercial Insurance Application"   # shown to the broker
    match:
      filename_patterns: ["*ACORD*125*", "*application*"]       # tried first
      content_keywords: ["Commercial Insurance Application"]    # then the document text
  - id: loss_runs
    description: "Loss runs for the past 5 years"
    match:
      filename_patterns: ["*loss*run*", "*claims*history*"]
      content_keywords: ["Loss Run", "Claims History"]
    requires_field:            # optional: check a value, not just presence
      name: years_covered
      type: number
      min_value: 5             # a 3-year loss run is reported as not enough
      unit: years              # wording used in the reply to the broker
escalation:
  threshold_hours: 72          # overrides the global setting for this line
```

`description` is what a broker reads, so write it the way you'd write it to
them. `requires_field` is the only check beyond "is the document here", and it
needs `ANTHROPIC_API_KEY` set to read the value — without a key it passes on the
document being present. Mistyped keys are rejected at startup rather than
silently ignored.

## Help

Questions, bugs, or "would this work for my agency?" — open a
[GitHub issue](https://github.com/atdayev/submission-triage/issues) or DM the
maintainer on LinkedIn.

## Contributing

Issues and pull requests welcome — for anything non-trivial, open an issue first
to discuss the approach.

CI runs gofmt, golangci-lint, and the tests on every PR; keep it green. Locally:

```bash
make test               # unit
make test-integration   # in-process IMAP + SMTP servers, plus the sample corpus
make test-stress        # concurrency and load, against a real database
```

The tests are the specification — most describe the behaviour they protect and
why it matters, so start there rather than reverse-engineering from the code.

## License

MIT. See [LICENSE](LICENSE).
