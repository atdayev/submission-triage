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
