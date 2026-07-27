package model

import (
	"fmt"
	"strings"
	"time"
)

// Digest section keys — the bucket a submission falls into.
const (
	sectionEscalated = "escalated"
	sectionAwaiting  = "awaiting"
	sectionComplete  = "complete"
	sectionUnknown   = "unknown"
)

// digestSections lists the digest's groups in display priority: sections the
// agency must act on first, complete at leisure, unknown-policy last.
var digestSections = []struct{ key, title string }{
	{sectionEscalated, "Escalated — broker went quiet"},
	{sectionAwaiting, "Awaiting the broker"},
	{sectionComplete, "Complete"},
	{sectionUnknown, "Unknown policy type"},
}

// BuildDigest renders the daily status digest: open submissions grouped by status
// priority with ages/outstanding items, a leading review count, flagged rows marked.
func BuildDigest(subs []Submission, filenameOnly map[string][]string, omitted int, now time.Time) string {
	if len(subs) == 0 {
		return ""
	}
	byKey := make(map[string][]Submission, len(digestSections))
	review := 0
	for _, s := range subs {
		if s.NeedsReview {
			review++
		}
		key := digestGroup(s)
		byKey[key] = append(byKey[key], s)
	}

	var b strings.Builder
	if review > 0 {
		fmt.Fprintf(&b, "%d submission(s) need review (marked below).\n\n", review)
	}
	b.WriteString("Open submissions:\n")
	for _, sec := range digestSections {
		group := byKey[sec.key]
		if len(group) == 0 {
			continue
		}
		showFilenameOnly := sec.key == sectionAwaiting || sec.key == sectionComplete
		fmt.Fprintf(&b, "\n%s (%d):\n", sec.title, len(group))
		for i := range group {
			var unverified []string
			if showFilenameOnly {
				unverified = filenameOnly[group[i].ID]
			}
			writeDigestRow(&b, group[i], unverified, now)
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "\n(+%d more open submissions omitted; raise DIGEST_MAX_ROWS to include them)\n", omitted)
	}
	return b.String()
}

// digestGroup buckets a submission by status priority; a policy-unknown case is
// grouped as unknown regardless of its (awaiting) state.
func digestGroup(s Submission) string {
	switch {
	case s.State == StateEscalated:
		return sectionEscalated
	case s.PolicyType == PolicyTypeUnknown:
		return sectionUnknown
	case s.State == StateComplete:
		return sectionComplete
	default:
		return sectionAwaiting
	}
}

func writeDigestRow(b *strings.Builder, s Submission, filenameOnly []string, now time.Time) {
	flag := ""
	if s.NeedsReview {
		flag = " [needs review]"
	}
	fmt.Fprintf(b, "  - %s | from %s | age %s | idle %s%s\n",
		digestSubject(s), digestFrom(s), humanDuration(now.Sub(s.CreatedAt)), humanDuration(now.Sub(s.LastActionAt)), flag)
	// outstanding items in customer wording (never internal item ids)
	for _, m := range s.MissingItems {
		if m.Reason != "" {
			fmt.Fprintf(b, "      %s: %s\n", m.Description, m.Reason)
		} else {
			fmt.Fprintf(b, "      %s\n", m.Description)
		}
	}
	for _, name := range filenameOnly {
		fmt.Fprintf(b, "      Matched on filename only, content not verified: %s\n", name)
	}
}

func digestSubject(s Submission) string {
	if strings.TrimSpace(s.SubjectLine) == "" {
		return "(no subject)"
	}
	return s.SubjectLine
}

func digestFrom(s Submission) string {
	if s.FromAddress == "" {
		return "(unknown sender)"
	}
	return s.FromAddress
}

// humanDuration renders a coarse age like "3h" or "2d".
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
