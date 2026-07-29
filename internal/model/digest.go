package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Digest section keys — the bucket a submission falls into.
const (
	sectionReplyBlocked   = "reply_blocked"
	sectionDeliveryFailed = "delivery_failed"
	sectionEscalated      = "escalated"
	sectionAwaiting       = "awaiting"
	sectionHeld           = "held"
	sectionComplete       = "complete"
	sectionUnknown        = "unknown"
)

// digestSections lists the digest's groups in display priority.
var digestSections = []struct{ key, title string }{
	{sectionReplyBlocked, "Reply withheld — REPLIES_ENABLED=false, nothing was sent"},
	{sectionDeliveryFailed, "Delivery failed — address unreachable, needs a human"},
	{sectionEscalated, "Escalated — broker went quiet"},
	{sectionAwaiting, "Awaiting the broker"},
	{sectionHeld, "On hold — paused by a human"},
	{sectionComplete, "Complete"},
	{sectionUnknown, "Unknown policy type"},
}

// BuildDigest renders the daily status digest: open submissions grouped by status
// priority. replyBlocked holds the ids whose reply the kill switch withheld — never
// resent automatically, so the digest is the only place they surface.
func BuildDigest(subs []Submission, filenameOnly map[string][]string, replyBlocked map[string]bool, omitted int, now time.Time) string {
	if len(subs) == 0 {
		return ""
	}
	byKey := make(map[string][]Submission, len(digestSections))
	review := 0
	for _, s := range subs {
		if s.NeedsReview {
			review++
		}
		key := digestGroup(s, replyBlocked[s.ID])
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
		// order the work queue by urgency
		sort.SliceStable(group, func(i, j int) bool { return bindLess(group[i], group[j]) })
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

// digestGroup buckets a submission by priority; flags win over state.
func digestGroup(s Submission, replyBlocked bool) string {
	switch {
	case replyBlocked:
		return sectionReplyBlocked
	case s.DeliveryFailed:
		return sectionDeliveryFailed
	case s.OnHold:
		return sectionHeld
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
	bind := ""
	if s.EffectiveDate != nil {
		bind = " | " + bindLabel(*s.EffectiveDate, now)
	}
	fmt.Fprintf(b, "  - %s | from %s | age %s | idle %s%s%s\n",
		digestSubject(s), digestFrom(s), humanDuration(now.Sub(s.CreatedAt)), humanDuration(now.Sub(s.LastActionAt)), bind, flag)
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

// digestSubject leads with the named insured, falling back to the subject line.
func digestSubject(s Submission) string {
	if ni := strings.TrimSpace(s.NamedInsured); ni != "" {
		return ni
	}
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

// bindLess orders submissions by effective date ascending, unknown dates last.
func bindLess(a, b Submission) bool {
	switch {
	case a.EffectiveDate != nil && b.EffectiveDate != nil:
		return a.EffectiveDate.Before(*b.EffectiveDate)
	case a.EffectiveDate != nil:
		return true
	default:
		return false
	}
}

// bindLabel renders days-to-bind, e.g. "binds in 3d", "binds today", "bound 2d ago".
// Whole UTC calendar days, not elapsed hours: a raw duration truncated to days puts
// both today and tomorrow in the same bucket and labels the whole 48h span "today".
func bindLabel(effective, now time.Time) string {
	days := int(utcMidnight(effective).Sub(utcMidnight(now)) / (24 * time.Hour))
	switch {
	case days < 0:
		return fmt.Sprintf("bound %dd ago", -days)
	case days == 0:
		return "binds today"
	default:
		return fmt.Sprintf("binds in %dd", days)
	}
}

func utcMidnight(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
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
