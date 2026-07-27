package oauth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/atdayev/submission-triage/pkg/retry"
)

func TestXOAUTH2Payload_ByteForByte(t *testing.T) {
	got := XOAUTH2Payload("user@example.com", "tok123")
	want := []byte("user=user@example.com\x01auth=Bearer tok123\x01\x01")
	if !bytes.Equal(got, want) {
		t.Fatalf("payload mismatch:\n got %q\nwant %q", got, want)
	}
	if n := bytes.Count(got, []byte{0x01}); n != 3 {
		t.Errorf("expected three 0x01 separators, got %d", n)
	}
	if !bytes.HasSuffix(got, []byte{0x01, 0x01}) {
		t.Error("payload must end with the two-0x01 trailing empty field")
	}
}

// stubSource is a fixed oauth2.TokenSource for exercising reuseSource.
type stubSource struct {
	tok *oauth2.Token
	err error
}

func (s stubSource) Token() (*oauth2.Token, error) { return s.tok, s.err }

func retrieveErr(code string, status int) error {
	return &oauth2.RetrieveError{ErrorCode: code, Response: &http.Response{StatusCode: status}}
}

func TestToken_InvalidGrantIsPermanentAuditedAndNotRetried(t *testing.T) {
	var gotProvider, gotClass string
	src := &reuseSource{
		provider: "google",
		src:      stubSource{err: retrieveErr("invalid_grant", http.StatusBadRequest)},
		onFail:   func(_ context.Context, provider, class string) { gotProvider, gotClass = provider, class },
	}

	var calls int
	err := retry.Do(context.Background(), 3, time.Millisecond, func(ctx context.Context) error {
		calls++
		_, e := src.Token(ctx)
		return e
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	var perm *retry.Permanent
	if !errors.As(err, &perm) {
		t.Errorf("invalid_grant should be permanent, got %T: %v", err, err)
	}
	if calls != 1 {
		t.Errorf("a permanent error must not be retried, got %d calls", calls)
	}
	if gotProvider != "google" || gotClass != "invalid_grant" {
		t.Errorf("onFail got provider=%q class=%q, want google/invalid_grant", gotProvider, gotClass)
	}
}

func TestToken_TransientErrorIsRetried(t *testing.T) {
	failed := false
	src := &reuseSource{provider: "microsoft", src: stubSource{err: errors.New("connection reset")}}
	src.onFail = func(context.Context, string, string) { failed = true }

	var calls int
	_ = retry.Do(context.Background(), 3, time.Millisecond, func(ctx context.Context) error {
		calls++
		_, e := src.Token(ctx)
		return e
	})
	if calls != 3 {
		t.Errorf("a transient token error should be retried to exhaustion, got %d calls", calls)
	}
	if failed {
		t.Error("onFail must fire only on permanent failures")
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		class     string
		permanent bool
	}{
		{"invalid_grant", retrieveErr("invalid_grant", 400), "invalid_grant", true},
		{"invalid_client", retrieveErr("invalid_client", 401), "invalid_client", true},
		{"other 400", retrieveErr("", 400), "invalid_request", true},
		{"server 500", retrieveErr("", 500), "transient", false},
		{"non-retrieve", errors.New("dial timeout"), "transient", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class, permanent := classify(tt.err)
			if class != tt.class || permanent != tt.permanent {
				t.Errorf("classify = (%q, %v), want (%q, %v)", class, permanent, tt.class, tt.permanent)
			}
		})
	}
}

func TestToken_SuccessReturnsAccessToken(t *testing.T) {
	src := &reuseSource{provider: "google", src: stubSource{tok: &oauth2.Token{AccessToken: "abc123"}}}
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc123" {
		t.Errorf("token: got %q, want abc123", got)
	}
}
