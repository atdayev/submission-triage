package emlparse

import (
	"strings"
	"testing"
)

func dsnMessage(status string) string {
	return strings.ReplaceAll(`From: Mail Delivery System <mailer-daemon@agency.example>
To: ops@agency.example
Subject: Delivery Status Notification (Failure)
Content-Type: multipart/report; report-type=delivery-status; boundary="xyz"

--xyz
Content-Type: text/plain

Your message could not be delivered.

--xyz
Content-Type: message/delivery-status

Reporting-MTA: dns; agency.example
Final-Recipient: rfc822; broker@nonexistent.example
Action: failed
Status: `+status+`

--xyz--
`, "\n", "\r\n")
}

func TestFromReader_HardBounce_Permanent(t *testing.T) {
	p, err := FromReader(strings.NewReader(dsnMessage("5.1.1")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !p.Bounce {
		t.Fatal("a multipart/report delivery-status must be detected as a bounce")
	}
	if !p.BouncePermanent {
		t.Error("a 5.x.x DSN status is a permanent bounce")
	}
}

func TestFromReader_SoftBounce_Transient(t *testing.T) {
	p, err := FromReader(strings.NewReader(dsnMessage("4.4.1")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !p.Bounce {
		t.Fatal("a 4.x.x DSN is still a bounce")
	}
	if p.BouncePermanent {
		t.Error("a 4.x.x DSN status is transient, not permanent")
	}
}

func TestFromReader_MailerDaemonSender_IsBounce(t *testing.T) {
	msg := strings.ReplaceAll(`From: postmaster@agency.example
To: ops@agency.example
Subject: Undeliverable
Return-Path: <>

Message could not be delivered.
`, "\n", "\r\n")
	p, err := FromReader(strings.NewReader(msg))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !p.Bounce {
		t.Error("a postmaster/mailer-daemon sender should be detected as a bounce")
	}
	if !p.BouncePermanent {
		t.Error("an unparseable DSN code defaults to permanent")
	}
}

func TestFromReader_NormalMail_NotBounce(t *testing.T) {
	msg := strings.ReplaceAll(`From: Broker <broker@example.com>
To: ops@agency.example
Subject: New Submission - CGL
Reply-To: producer@example.com

Please find the ACORD 125 attached.
`, "\n", "\r\n")
	p, err := FromReader(strings.NewReader(msg))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Bounce {
		t.Error("normal broker mail must not be flagged as a bounce")
	}
	if p.ReplyTo != "producer@example.com" {
		t.Errorf("Reply-To not surfaced: got %q", p.ReplyTo)
	}
}

// Compliant DSNs carry In-Reply-To and thread on their own; non-compliant ones
// carry only an embedded copy of the failed message, and without reading its
// Message-ID those bounces attach to nothing.
func TestFromReader_ReadsBouncedMessageIDFromEmbeddedCopy(t *testing.T) {
	const dsn = "From: Mail Delivery System <mailer-daemon@relay.example>\r\n" +
		"To: ops@agency.example\r\n" +
		"Subject: Undeliverable\r\n" +
		"Content-Type: multipart/report; report-type=delivery-status; boundary=\"bb\"\r\n" +
		"\r\n" +
		"--bb\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"Your message could not be delivered.\r\n" +
		"--bb\r\n" +
		"Content-Type: message/delivery-status\r\n" +
		"\r\n" +
		"Status: 5.1.1\r\n" +
		"--bb\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"\r\n" +
		"From: ops@agency.example\r\n" +
		"To: broker@x.example\r\n" +
		"Message-ID: <reply-9@agency.example>\r\n" +
		"Subject: Re: New Submission\r\n" +
		"\r\n" +
		"Message-ID: <decoy@nowhere.example>\r\n" +
		"--bb--\r\n"

	p, err := FromReader(strings.NewReader(dsn))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !p.Bounce || !p.BouncePermanent {
		t.Fatalf("expected a permanent bounce, got bounce=%v permanent=%v", p.Bounce, p.BouncePermanent)
	}
	if p.BouncedMessageID != "reply-9@agency.example" {
		t.Errorf("bounced message id: got %q, want reply-9@agency.example", p.BouncedMessageID)
	}
}

// Only the embedded headers are read; a Message-ID quoted in the body below them is
// not ours to thread on.
func TestFromReader_IgnoresMessageIDInBouncedBody(t *testing.T) {
	const dsn = "From: postmaster@relay.example\r\n" +
		"Subject: Undeliverable\r\n" +
		"Content-Type: multipart/report; report-type=delivery-status; boundary=\"bb\"\r\n" +
		"\r\n" +
		"--bb\r\n" +
		"Content-Type: message/delivery-status\r\n" +
		"\r\n" +
		"Status: 5.1.1\r\n" +
		"--bb\r\n" +
		"Content-Type: text/rfc822-headers\r\n" +
		"\r\n" +
		"From: ops@agency.example\r\n" +
		"Subject: Re: New Submission\r\n" +
		"\r\n" +
		"Message-ID: <in-the-body@nowhere.example>\r\n" +
		"--bb--\r\n"

	p, err := FromReader(strings.NewReader(dsn))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.BouncedMessageID != "" {
		t.Errorf("a Message-ID below the header block must not be used, got %q", p.BouncedMessageID)
	}
}
