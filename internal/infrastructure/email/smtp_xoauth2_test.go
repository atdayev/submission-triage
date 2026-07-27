package email

import (
	"context"
	"errors"
	"net/smtp"
	"testing"

	"github.com/atdayev/submission-triage/internal/infrastructure/oauth"
)

// stubTokenSource is a fixed oauth.TokenSource for the SMTP auth tests.
type stubTokenSource struct {
	token string
	err   error
}

func (s stubTokenSource) Token(context.Context) (string, error) { return s.token, s.err }

func TestXOAUTH2Auth_StartEmitsInitialResponse(t *testing.T) {
	mech, err := XOAUTH2Auth("ops@agency.example", stubTokenSource{token: "tok"})(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	name, ir, err := mech.Start(&smtp.ServerInfo{Name: "smtp.example.com", TLS: true})
	if err != nil {
		t.Fatal(err)
	}
	if name != "XOAUTH2" {
		t.Errorf("mechanism: got %q, want XOAUTH2", name)
	}
	if want := string(oauth.XOAUTH2Payload("ops@agency.example", "tok")); string(ir) != want {
		t.Errorf("initial response: got %q, want %q", ir, want)
	}
}

func TestXOAUTH2Auth_RequiresTLSExceptLoopback(t *testing.T) {
	mech := xoauth2Auth{user: "u@x", token: "tok"}
	if _, _, err := mech.Start(&smtp.ServerInfo{Name: "smtp.example.com", TLS: false}); err == nil {
		t.Error("a bearer token must not cross a plaintext remote connection")
	}
	if _, _, err := mech.Start(&smtp.ServerInfo{Name: "localhost", TLS: false}); err != nil {
		t.Errorf("loopback should be exempt: %v", err)
	}
	if _, _, err := mech.Start(&smtp.ServerInfo{Name: "smtp.example.com", TLS: true}); err != nil {
		t.Errorf("TLS should be accepted: %v", err)
	}
}

func TestXOAUTH2Auth_NextRejectsUnexpectedChallenge(t *testing.T) {
	mech := xoauth2Auth{user: "u@x", token: "tok"}
	// a continuation only arrives when auth failed; we do not answer it
	if _, err := mech.Next([]byte("eyJzdGF0dXMiOiI0MDEifQ=="), true); err == nil {
		t.Error("expected an error on an unexpected server challenge")
	}
	if resp, err := mech.Next(nil, false); err != nil || resp != nil {
		t.Errorf("terminal Next: got (%v, %v), want (nil, nil)", resp, err)
	}
}

func TestXOAUTH2Auth_TokenErrorPropagates(t *testing.T) {
	if _, err := XOAUTH2Auth("u@x", stubTokenSource{err: errors.New("token dead")})(context.Background()); err == nil {
		t.Error("expected the token error to propagate")
	}
}
