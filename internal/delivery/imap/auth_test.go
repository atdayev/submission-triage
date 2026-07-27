package imap

import (
	"context"
	"errors"
	"testing"

	"github.com/atdayev/submission-triage/internal/infrastructure/oauth"
)

// stubTokenSource is a fixed oauth.TokenSource for the IMAP auth tests.
type stubTokenSource struct {
	token string
	err   error
}

func (s stubTokenSource) Token(context.Context) (string, error) { return s.token, s.err }

func TestXOAUTH2Client_StartPayload(t *testing.T) {
	c := &xoauth2Client{user: "ops@agency.example", token: "tok"}
	mech, ir, err := c.Start()
	if err != nil {
		t.Fatal(err)
	}
	if mech != "XOAUTH2" {
		t.Errorf("mechanism: got %q, want XOAUTH2", mech)
	}
	if want := string(oauth.XOAUTH2Payload("ops@agency.example", "tok")); string(ir) != want {
		t.Errorf("initial response: got %q, want %q", ir, want)
	}
}

func TestXOAUTH2Client_NextAborts(t *testing.T) {
	c := &xoauth2Client{user: "u", token: "t"}
	if _, err := c.Next([]byte("base64-error-challenge")); err == nil {
		t.Error("Next must abort on a server challenge (auth already failed)")
	}
}

func TestXOAUTH2Auth_TokenErrorPropagates(t *testing.T) {
	auth := XOAUTH2Auth("u@x", stubTokenSource{err: errors.New("no token")})
	if err := auth(context.Background(), nil); err == nil {
		t.Error("expected the token error to propagate before touching the client")
	}
}
