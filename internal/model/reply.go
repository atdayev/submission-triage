package model

import (
	"fmt"
	"strings"
)

type Reply struct {
	SubmissionID string
	ToAddress    string
	Subject      string
	BodyText     string
	InReplyTo    string
	References   []string
}

func BuildMissingItemsReply(s Submission, missing []MissingItem, lastInbound Email) Reply {
	var b strings.Builder
	fmt.Fprintf(&b, "Hi %s,\n\n", greetingName(lastInbound))
	b.WriteString("Thanks for the submission. To finish the file we still need:\n\n")
	for _, m := range missing {
		if m.Reason != "" && m.Reason != "document not provided" {
			fmt.Fprintf(&b, "  - %s (%s)\n", m.Description, m.Reason)
			continue
		}
		fmt.Fprintf(&b, "  - %s\n", m.Description)
	}
	b.WriteString("\nReply to this thread with the documents and we'll continue.\n")
	return newReply(s, b.String(), lastInbound)
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

func BuildCompletionReply(s Submission, lastInbound Email) Reply {
	body := fmt.Sprintf("Hi %s,\n\n"+
		"Thanks — we now have everything we need on this submission. "+
		"It's moving to underwriting; you'll hear back from us shortly.\n",
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
		ToAddress:    lastInbound.FromAddress,
		Subject:      subject,
		BodyText:     body,
		InReplyTo:    lastInbound.MessageID,
		References:   refs,
	}
}

func greetingName(e Email) string {
	if e.FromName != "" {
		parts := strings.Fields(e.FromName)
		if len(parts) > 0 {
			return parts[0]
		}
	}
	if at := strings.Index(e.FromAddress, "@"); at > 0 {
		if local := e.FromAddress[:at]; usableGreeting(local) {
			return local
		}
	}
	return "there"
}

var roleLocalParts = map[string]bool{
	"submissions": true,
	"info":        true,
	"noreply":     true,
	"no-reply":    true,
	"support":     true,
}

// usableGreeting reports whether an email local-part reads as a first name.
// Digit-bearing handles, very short ones, and role addresses fall back to
// "there" rather than greeting "Hi noreply,".
func usableGreeting(local string) bool {
	if len(local) < 3 {
		return false
	}
	if strings.ContainsAny(local, "0123456789") {
		return false
	}
	return !roleLocalParts[strings.ToLower(local)]
}
