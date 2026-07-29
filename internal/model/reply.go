package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Reply is an outbound email the service sends in-thread.
type Reply struct {
	SubmissionID string
	ToAddress    string
	Subject      string
	BodyText     string
	InReplyTo    string
	References   []string
}

// ReplyKind is which of the three replies a submission would receive.
type ReplyKind string

const (
	ReplyKindPolicyUnknown ReplyKind = "policy_unknown"
	ReplyKindCompletion    ReplyKind = "completion"
	ReplyKindMissingItems  ReplyKind = "missing_items"
)

// ReplyFingerprint hashes what a reply would tell the broker: its kind plus the
// sorted item ids and reason codes behind it. An identical fingerprint means the
// reply would repeat itself.
func ReplyFingerprint(kind ReplyKind, items []MissingItem) string {
	keys := make([]string, 0, len(items))
	for _, m := range items {
		keys = append(keys, m.ID+"\x1f"+string(m.Code))
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(kind))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
	}
	return hex.EncodeToString(h.Sum(nil))
}

const (
	missingAttachmentLine = "We didn't find any attachments on this message. If you sent a download link, could you attach the files directly instead?"
	encryptedLineFmt      = "We received %s but it's password-protected and we can't open it. Could you resend it unlocked, or send the password separately?"
)

// BuildMissingItemsReply lists the outstanding documents for the sender, given only
// broker-actionable items. A missing-attachment signal supersedes the list; each
// password-protected item gets its own resend-unlocked ask.
func BuildMissingItemsReply(s Submission, missing []MissingItem, lastInbound Email) Reply {
	var b strings.Builder
	fmt.Fprintf(&b, "Hi %s,\n\n", greetingName(lastInbound))

	if hasCode(missing, ReasonMissingAttachment) {
		b.WriteString(missingAttachmentLine + "\n")
		return newReply(s, b.String(), lastInbound)
	}

	var absent, encrypted []MissingItem
	for _, m := range missing {
		if m.Code == ReasonEncrypted {
			encrypted = append(encrypted, m)
		} else {
			absent = append(absent, m)
		}
	}
	if len(absent) > 0 {
		b.WriteString("Thanks for the submission. To finish the file we still need:\n\n")
		for _, m := range absent {
			if m.Code == ReasonFieldShortfall {
				fmt.Fprintf(&b, "  - %s (%s)\n", m.Description, m.Reason)
			} else {
				fmt.Fprintf(&b, "  - %s\n", m.Description)
			}
		}
	}
	for _, m := range encrypted {
		fmt.Fprintf(&b, "\n"+encryptedLineFmt+"\n", m.Description)
	}
	b.WriteString("\nReply to this thread with the documents and we'll continue.\n")
	return newReply(s, b.String(), lastInbound)
}

func hasCode(missing []MissingItem, code ReasonCode) bool {
	for _, m := range missing {
		if m.Code == code {
			return true
		}
	}
	return false
}

// BuildPolicyUnknownReply asks the sender to name the line of business;
// knownTypes are offered as examples.
func BuildPolicyUnknownReply(s Submission, lastInbound Email, knownTypes []string) Reply {
	hint := "the line of business"
	if len(knownTypes) > 0 {
		hint = "the line of business (e.g., " + strings.Join(knownTypes, ", ") + ")"
	}
	body := fmt.Sprintf("Hi %s,\n\n"+
		"Thanks for the submission. We couldn't determine the policy type "+
		"from the subject line. Could you reply with %s so we can pull the "+
		"right checklist?\n",
		greetingName(lastInbound), hint)
	return newReply(s, body, lastInbound)
}

// BuildCompletionReply confirms the submission is complete, reporting only what
// we observe — never promising a downstream underwriting action.
func BuildCompletionReply(s Submission, lastInbound Email) Reply {
	body := fmt.Sprintf("Hi %s,\n\n"+
		"Thanks — everything on our checklist for this submission is now accounted for. "+
		"Nothing further is needed from you at this time.\n",
		greetingName(lastInbound))
	return newReply(s, body, lastInbound)
}

func newReply(s Submission, body string, lastInbound Email) Reply {
	subject := lastInbound.Subject
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	refs := append([]string{}, lastInbound.References...)
	if lastInbound.MessageID != "" {
		refs = append(refs, lastInbound.MessageID)
	}
	return Reply{
		SubmissionID: s.ID,
		ToAddress:    replyTarget(lastInbound),
		Subject:      subject,
		BodyText:     body,
		InReplyTo:    lastInbound.MessageID,
		References:   refs,
	}
}

// replyTarget picks where a reply goes: Reply-To when the sender set one (an
// assistant or shared inbox sends From, but the producer owns Reply-To), else
// From. CC is deliberately not copied — keeping the original CC list in the loop
// risks replying to a wide distribution.
func replyTarget(e Email) string {
	if rt := strings.TrimSpace(e.ReplyTo); rt != "" {
		return rt
	}
	return e.FromAddress
}

func greetingName(e Email) string {
	if e.FromName != "" {
		parts := strings.Fields(e.FromName)
		if len(parts) > 0 {
			if name := sanitizeGreeting(parts[0]); name != "" {
				return name
			}
		}
	}
	if at := strings.Index(e.FromAddress, "@"); at > 0 {
		if local := e.FromAddress[:at]; usableGreeting(local) {
			return local
		}
	}
	return "there"
}

// sanitizeGreeting drops control characters from a name.
func sanitizeGreeting(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

var roleLocalParts = map[string]bool{
	"submissions": true,
	"info":        true,
	"noreply":     true,
	"no-reply":    true,
	"support":     true,
}

// usableGreeting reports whether an email local-part reads as a first name.
func usableGreeting(local string) bool {
	if len(local) < 3 {
		return false
	}
	for _, r := range local {
		if r < 0x20 || r == 0x7f {
			return false // control char from an unparseable From address
		}
	}
	if strings.ContainsAny(local, "0123456789") {
		return false
	}
	return !roleLocalParts[strings.ToLower(local)]
}
