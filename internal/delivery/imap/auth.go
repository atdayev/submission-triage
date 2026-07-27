package imap

import (
	"context"
	"fmt"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/atdayev/submission-triage/internal/infrastructure/oauth"
)

// Authenticator logs a freshly-dialed client into the server.
type Authenticator func(ctx context.Context, c *imapclient.Client) error

// PasswordAuth authenticates with the IMAP LOGIN command.
func PasswordAuth(username, password string) Authenticator {
	return func(_ context.Context, c *imapclient.Client) error {
		return c.Login(username, password).Wait()
	}
}

// XOAUTH2Auth authenticates with the XOAUTH2 SASL mechanism, minting a fresh
// token per connection.
func XOAUTH2Auth(user string, ts oauth.TokenSource) Authenticator {
	return func(ctx context.Context, c *imapclient.Client) error {
		token, err := ts.Token(ctx)
		if err != nil {
			return err
		}
		return c.Authenticate(&xoauth2Client{user: user, token: token})
	}
}

// xoauth2Client is a minimal sasl.Client for XOAUTH2 (go-sasl ships OAUTHBEARER
// but not XOAUTH2).
type xoauth2Client struct {
	user  string
	token string
}

func (a *xoauth2Client) Start() (mech string, ir []byte, err error) {
	return "XOAUTH2", oauth.XOAUTH2Payload(a.user, a.token), nil
}

// Next only runs when the server rejects the token and returns a base64 error
// challenge; we do not answer it, so aborting here fails the doomed exchange.
func (a *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	return nil, fmt.Errorf("imap: XOAUTH2 authentication rejected")
}
