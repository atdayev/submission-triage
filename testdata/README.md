# testdata

Synthetic, non-real submission emails used by the end-to-end test in
`internal/delivery/imap/e2e_test.go`.

**Nothing here represents a real submission, real insured, or real broker.**
All names, addresses, and claim narratives are fabricated. The "PDF"
attachments are deliberately minimal — base64-encoded plain text. The PDF
extractor refuses non-`%PDF` byte streams and logs a warning, but the
service still classifies the attachment by filename glob and records it
in the document table, so the corpus end-to-end shapes the same final
state regardless of whether text extraction succeeded.

## Contents

`eml/` — 12 `.eml` files broken into four scenario buckets:

| Bucket | Files | Expected outcome |
|--------|-------|------------------|
| New submission, all docs present | `01_*` `02_*` `03_*` | `complete` |
| New submission, 1–3 docs missing | `04_*` `05_*` `06_*` `07_*` | `awaiting`, reply with missing list |
| Follow-up with missing docs attached | `08_*` `09_*` `10_*` | re-threads onto the original, transitions to `complete` |
| Stale, dated past the escalation threshold | `11_*` `12_*` | hits `escalated` on the next worker tick |

## Regenerating

The `.eml` files are checked in. Edit them by hand to add a new scenario.
If you change a `Message-ID`, update any follow-up file that references
it in its `In-Reply-To` / `References` headers so the threading test
still works.
