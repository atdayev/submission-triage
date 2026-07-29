// Package oauth mints XOAUTH2 access tokens for IMAP and SMTP, refreshed as
// needed, for Google (stored refresh token) and Microsoft 365 (client
// credentials). One token source serves both protocols.
package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/pkg/retry"
)

const (
	googleAuthURL   = "https://accounts.google.com/o/oauth2/auth"
	googleTokenURL  = "https://oauth2.googleapis.com/token"
	googleMailScope = "https://mail.google.com/"

	microsoftTokenURLTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"
	microsoftScope            = "https://outlook.office365.com/.default"
)

const tokenFetchTimeout = 30 * time.Second

// TokenSource yields a valid access token, refreshing as needed.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// FailureFunc records a permanent auth failure (invalid_grant, revoked consent,
// expired secret). The token is never passed.
type FailureFunc func(ctx context.Context, provider, class string)

// XOAUTH2Payload builds the SASL initial-response shared by IMAP and SMTP:
// "user=<addr>^Aauth=Bearer <token>^A^A" (^A is 0x01), unencoded — the callers'
// SASL/AUTH layers base64-encode it.
func XOAUTH2Payload(user, token string) []byte {
	return []byte("user=" + user + "\x01auth=Bearer " + token + "\x01\x01")
}

// NewTokenSource builds the provider-appropriate token source. onFail, if
// non-nil, fires when a token fetch fails permanently.
func NewTokenSource(cfg config.OAuthConfig, onFail FailureFunc) (TokenSource, error) {
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: tokenFetchTimeout})
	switch strings.ToLower(cfg.Provider) {
	case config.OAuthProviderMicrosoft:
		cc := &clientcredentials.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			TokenURL:     fmt.Sprintf(microsoftTokenURLTemplate, cfg.TenantID),
			Scopes:       []string{microsoftScope},
		}
		return &reuseSource{provider: config.OAuthProviderMicrosoft, src: cc.TokenSource(ctx), onFail: onFail}, nil
	case config.OAuthProviderGoogle:
		oc := &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     oauth2.Endpoint{AuthURL: googleAuthURL, TokenURL: googleTokenURL},
			Scopes:       []string{googleMailScope},
		}
		src := oc.TokenSource(ctx, &oauth2.Token{RefreshToken: cfg.RefreshToken})
		return &reuseSource{provider: config.OAuthProviderGoogle, src: src, onFail: onFail}, nil
	default:
		return nil, fmt.Errorf("oauth: unknown provider %q", cfg.Provider)
	}
}

// GoogleConsentConfig returns the oauth2 config for the one-time Google consent
// flow that mints a refresh token (see cmd/authorize).
func GoogleConsentConfig(clientID, clientSecret, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: googleAuthURL, TokenURL: googleTokenURL},
		Scopes:       []string{googleMailScope},
		RedirectURL:  redirectURL,
	}
}

// reuseSource adapts a caching oauth2.TokenSource, returning the bare access
// token and classifying permanent failures for pkg/retry.
type reuseSource struct {
	provider string
	src      oauth2.TokenSource
	onFail   FailureFunc
}

// Token returns a fresh access token, or a retry.Permanent error when the
// failure will never recover (revoked consent, bad refresh token, dead secret).
func (r *reuseSource) Token(ctx context.Context) (string, error) {
	tok, err := r.src.Token()
	if err != nil {
		if class, permanent := classify(err); permanent {
			if r.onFail != nil {
				r.onFail(ctx, r.provider, class)
			}
			return "", retry.MarkPermanent(fmt.Errorf("oauth %s: %w", r.provider, err))
		}
		return "", fmt.Errorf("oauth %s: %w", r.provider, err)
	}
	return tok.AccessToken, nil
}

// classify reports the token error's class and whether it is permanent.
func classify(err error) (class string, permanent bool) {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return "transient", false
	}
	switch re.ErrorCode {
	case "invalid_grant", "invalid_client", "unauthorized_client", "invalid_scope":
		return re.ErrorCode, true
	}
	if re.Response != nil && re.Response.StatusCode == http.StatusBadRequest {
		if re.ErrorCode != "" {
			return re.ErrorCode, true
		}
		return "invalid_request", true
	}
	if re.ErrorCode != "" {
		return re.ErrorCode, false
	}
	return "transient", false
}
