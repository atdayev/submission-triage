//go:build integration

package imap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
)

// These tests exercise the real imapMailbox adapter against an in-process go-imap server.

const (
	testUser = "alice@example.com"
	testPass = "app-password"
)

const secondEML = "From: Bob <bob@example.com>\r\n" +
	"To: ops@agency.example\r\n" +
	"Subject: New Submission - BOP\r\n" +
	"Message-ID: <m2@example.com>\r\n" +
	"Date: Tue, 20 May 2026 10:00:00 -0400\r\n" +
	"\r\n" +
	"Second submission body.\r\n"

// startMemServer seeds an in-process IMAP INBOX with the given unseen messages.
func startMemServer(t *testing.T, msgs ...string) string {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(testUser, testPass)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(_ *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		// advertise MOVE + UIDPLUS so the adapter's capability ladder can take the MOVE path
		Caps: goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapMove: {}, goimap.CapUIDPlus: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	seed, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatalf("seed dial: %v", err)
	}
	defer seed.Close()
	if err := seed.Login(testUser, testPass).Wait(); err != nil {
		t.Fatalf("seed login: %v", err)
	}
	for _, raw := range msgs {
		cmd := seed.Append("INBOX", int64(len(raw)), nil)
		if _, err := cmd.Write([]byte(raw)); err != nil {
			t.Fatalf("append write: %v", err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatalf("append close: %v", err)
		}
		if _, err := cmd.Wait(); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	_ = seed.Logout().Wait()
	return ln.Addr().String()
}

// useInsecureDial points the adapter's transport at the plaintext test server.
func useInsecureDial(t *testing.T) {
	t.Helper()
	orig := dialClient
	dialClient = func(addr string) (*imapclient.Client, error) {
		return imapclient.DialInsecure(addr, nil)
	}
	t.Cleanup(func() { dialClient = orig })
}

func testLog() *logrus.Entry { return logrus.NewEntry(logrus.New()) }

func passwordAuthFor(cfg config.IMAPConfig) Authenticator {
	return PasswordAuth(cfg.Username, cfg.Password)
}

func cfgFor(addr string) config.IMAPConfig {
	host, port, _ := net.SplitHostPort(addr)
	return config.IMAPConfig{
		Host:                host,
		Port:                port,
		Username:            testUser,
		Password:            testPass,
		Mailbox:             "INBOX",
		PollIntervalSeconds: 30,
	}
}

func TestIntegration_RealAdapter_FetchMarkSeenRefetch(t *testing.T) {
	addr := startMemServer(t, validEML, secondEML)
	useInsecureDial(t)
	ctx := context.Background()

	mb, err := dialIMAP(cfgFor(addr), passwordAuthFor(cfgFor(addr)), testLog())(ctx)
	if err != nil {
		t.Fatalf("dialIMAP (dial+login+select): %v", err)
	}
	defer mb.Close()

	msgs, err := mb.FetchUnseen(ctx, 50)
	if err != nil {
		t.Fatalf("FetchUnseen: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("unseen count: got %d, want 2", len(msgs))
	}

	// BODY.PEEK[] + FindBodySection must round-trip the raw message bytes
	var foundM1 bool
	for _, m := range msgs {
		if m.UID == 0 {
			t.Error("message has zero UID")
		}
		if len(m.Raw) == 0 {
			t.Errorf("uid %d: empty raw body — FindBodySection returned nil", m.UID)
		}
		if strings.Contains(string(m.Raw), "Message-ID: <m1@example.com>") {
			foundM1 = true
		}
	}
	if !foundM1 {
		t.Error("did not recover message m1's raw content")
	}

	// marking one seen drops it from the unseen set; the other stays
	if err := mb.MarkSeen(ctx, msgs[0].UID); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	remaining, err := mb.FetchUnseen(ctx, 50)
	if err != nil {
		t.Fatalf("re-FetchUnseen: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("after mark-seen unseen count: got %d, want 1", len(remaining))
	}
	if remaining[0].UID == msgs[0].UID {
		t.Errorf("message %d marked seen but still returned as unseen", msgs[0].UID)
	}
}

func TestIntegration_RealAdapter_FileMovesMessage(t *testing.T) {
	addr := startMemServer(t, validEML)
	useInsecureDial(t)
	ctx := context.Background()

	// imapmemserver advertises MOVE, so File must take the MOVE path end to end
	mb, err := dialIMAP(cfgFor(addr), passwordAuthFor(cfgFor(addr)), testLog())(ctx)
	if err != nil {
		t.Fatalf("dialIMAP: %v", err)
	}
	defer mb.Close()

	msgs, err := mb.FetchUnseen(ctx, 50)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("FetchUnseen: got %d (err %v), want 1", len(msgs), err)
	}

	const folder = "Ready for Underwriting"
	// folder doesn't exist yet: File must create it, then MOVE into it
	if err := mb.File(ctx, msgs[0].UID, folder); err != nil {
		t.Fatalf("File (create+move): %v", err)
	}

	c := mb.(*imapMailbox).c
	// the message is now in the folder...
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		t.Fatalf("select folder %q: %v", folder, err)
	}
	found, err := c.UIDSearch(&goimap.SearchCriteria{}, nil).Wait()
	if err != nil {
		t.Fatalf("search folder: %v", err)
	}
	if n := len(found.AllUIDs()); n != 1 {
		t.Errorf("message not moved into %q: %d found, want 1", folder, n)
	}
	// ...and MOVE removed it from the inbox (no duplicate, unlike COPY)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("re-select INBOX: %v", err)
	}
	inbox, err := c.UIDSearch(&goimap.SearchCriteria{}, nil).Wait()
	if err != nil {
		t.Fatalf("search inbox: %v", err)
	}
	if n := len(inbox.AllUIDs()); n != 0 {
		t.Errorf("MOVE left a duplicate in the inbox: %d messages, want 0", n)
	}
}

func TestIntegration_RealAdapter_RespectsBatchLimit(t *testing.T) {
	addr := startMemServer(t, validEML, secondEML)
	useInsecureDial(t)

	mb, err := dialIMAP(cfgFor(addr), passwordAuthFor(cfgFor(addr)), testLog())(context.Background())
	if err != nil {
		t.Fatalf("dialIMAP: %v", err)
	}
	defer mb.Close()

	msgs, err := mb.FetchUnseen(context.Background(), 1)
	if err != nil {
		t.Fatalf("FetchUnseen: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("batch limit not honored: got %d, want 1", len(msgs))
	}
}

func TestIntegration_PollerEndToEnd(t *testing.T) {
	addr := startMemServer(t, validEML, secondEML)
	useInsecureDial(t)

	ing := &fakeIngester{}
	p := &Poller{
		dial:       dialIMAP(cfgFor(addr), passwordAuthFor(cfgFor(addr)), testLog()),
		ingest:     ing,
		interval:   time.Hour,
		batchLimit: 50,
		mailbox:    "INBOX",
		log:        logrus.NewEntry(logrus.New()),
	}

	p.pollOnce(context.Background())
	if len(ing.reqs) != 2 {
		t.Fatalf("first poll ingested %d, want 2", len(ing.reqs))
	}
	for _, r := range ing.reqs {
		if r.Source != "imap" {
			t.Errorf("source: got %q, want imap", r.Source)
		}
	}

	// all marked seen, so a second poll finds nothing: the full loop works
	ing.reqs = nil
	p.pollOnce(context.Background())
	if len(ing.reqs) != 0 {
		t.Errorf("second poll re-ingested %d messages; mark-seen didn't stick", len(ing.reqs))
	}
}

func TestIntegration_DialFailsOnBadCredentials(t *testing.T) {
	addr := startMemServer(t)
	useInsecureDial(t)

	cfg := cfgFor(addr)
	cfg.Password = "wrong"
	if _, err := dialIMAP(cfg, passwordAuthFor(cfg), testLog())(context.Background()); err == nil {
		t.Fatal("expected login failure with wrong password")
	}
}

func TestIntegration_CancelClosesConnection(t *testing.T) {
	addr := startMemServer(t, validEML)
	useInsecureDial(t)

	ctx, cancel := context.WithCancel(context.Background())
	mb, err := dialIMAP(cfgFor(addr), passwordAuthFor(cfgFor(addr)), testLog())(ctx)
	if err != nil {
		t.Fatalf("dialIMAP: %v", err)
	}
	defer mb.Close()

	cancel() // shutdown: the watcher must close the connection so commands unblock
	select {
	case <-mb.(*imapMailbox).c.Closed():
	case <-time.After(2 * time.Second):
		t.Fatal("connection not closed after ctx cancel; in-flight commands could hang at shutdown")
	}
}

// xoauth2MemSession wraps a memserver session to accept the XOAUTH2 mechanism
// with a fixed token; imapmemserver itself supports only LOGIN.
type xoauth2MemSession struct {
	imapserver.Session
	token string
}

func (s *xoauth2MemSession) AuthenticateMechanisms() []string { return []string{"XOAUTH2"} }

func (s *xoauth2MemSession) Authenticate(mech string) (sasl.Server, error) {
	if mech != "XOAUTH2" {
		return nil, fmt.Errorf("unsupported mechanism %q", mech)
	}
	return &xoauth2MemServer{token: s.token, inner: s.Session}, nil
}

// xoauth2MemServer validates the XOAUTH2 payload, then binds the memserver user
// via its underlying LOGIN (the test password is known).
type xoauth2MemServer struct {
	token string
	inner imapserver.Session
}

func (s *xoauth2MemServer) Next(response []byte) (challenge []byte, done bool, err error) {
	user, token, ok := parseXOAUTH2(response)
	if !ok {
		return nil, false, errors.New("malformed XOAUTH2 payload")
	}
	if token != s.token {
		return nil, false, errors.New("invalid token")
	}
	if err := s.inner.Login(user, testPass); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// parseXOAUTH2 pulls the user and bearer token out of "user=<u>\x01auth=Bearer <t>\x01\x01".
func parseXOAUTH2(payload []byte) (user, token string, ok bool) {
	for _, field := range strings.Split(string(payload), "\x01") {
		switch {
		case strings.HasPrefix(field, "user="):
			user = strings.TrimPrefix(field, "user=")
		case strings.HasPrefix(field, "auth=Bearer "):
			token = strings.TrimPrefix(field, "auth=Bearer ")
		}
	}
	return user, token, user != "" && token != ""
}

// startXOAUTH2MemServer is startMemServer with sessions that accept XOAUTH2.
func startXOAUTH2MemServer(t *testing.T, token string, msgs ...string) string {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(testUser, testPass)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(_ *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &xoauth2MemSession{Session: mem.NewSession(), token: token}, nil, nil
		},
		InsecureAuth: true,
		// the session wrapper hides imapmemserver's optional interfaces (MOVE, etc.),
		// so advertise only core IMAP4rev1 — this test exercises auth + fetch
		Caps: goimap.CapSet{goimap.CapIMAP4rev1: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	seed, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatalf("seed dial: %v", err)
	}
	defer seed.Close()
	if err := seed.Login(testUser, testPass).Wait(); err != nil {
		t.Fatalf("seed login: %v", err)
	}
	for _, raw := range msgs {
		cmd := seed.Append("INBOX", int64(len(raw)), nil)
		if _, err := cmd.Write([]byte(raw)); err != nil {
			t.Fatalf("append write: %v", err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatalf("append close: %v", err)
		}
		if _, err := cmd.Wait(); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	_ = seed.Logout().Wait()
	return ln.Addr().String()
}

func TestIntegration_RealAdapter_XOAUTH2Auth(t *testing.T) {
	const token = "ya29.stub-access-token"
	addr := startXOAUTH2MemServer(t, token, validEML)
	useInsecureDial(t)
	ctx := context.Background()

	auth := XOAUTH2Auth(testUser, stubTokenSource{token: token})
	mb, err := dialIMAP(cfgFor(addr), auth, testLog())(ctx)
	if err != nil {
		t.Fatalf("dialIMAP with XOAUTH2: %v", err)
	}
	defer mb.Close()

	msgs, err := mb.FetchUnseen(ctx, 50)
	if err != nil {
		t.Fatalf("FetchUnseen after XOAUTH2 auth: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("unseen count: got %d, want 1", len(msgs))
	}
}

func TestIntegration_RealAdapter_XOAUTH2BadTokenFails(t *testing.T) {
	addr := startXOAUTH2MemServer(t, "the-right-token", validEML)
	useInsecureDial(t)

	auth := XOAUTH2Auth(testUser, stubTokenSource{token: "the-wrong-token"})
	if _, err := dialIMAP(cfgFor(addr), auth, testLog())(context.Background()); err == nil {
		t.Fatal("expected XOAUTH2 auth to fail with a bad token")
	}
}

func TestIntegration_RealAdapter_SkipsOversized(t *testing.T) {
	big := "From: Big <big@example.com>\r\n" +
		"To: ops@agency.example\r\n" +
		"Subject: Huge\r\n" +
		"Message-ID: <big@example.com>\r\n" +
		"Date: Tue, 20 May 2026 10:00:00 -0400\r\n" +
		"\r\n" + strings.Repeat("A", 2<<20) + "\r\n" // ~2 MiB body
	addr := startMemServer(t, validEML, big)
	useInsecureDial(t)

	cfg := cfgFor(addr)
	cfg.MaxMessageMB = 1 // 1 MiB cap; the big message must be skipped
	mb, err := dialIMAP(cfg, passwordAuthFor(cfg), testLog())(context.Background())
	if err != nil {
		t.Fatalf("dialIMAP: %v", err)
	}
	defer mb.Close()

	msgs, err := mb.FetchUnseen(context.Background(), 50)
	if err != nil {
		t.Fatalf("FetchUnseen: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("over-cap message not skipped: got %d, want 1", len(msgs))
	}
	if !strings.Contains(string(msgs[0].Raw), "Message-ID: <m1@example.com>") {
		t.Error("expected only the small message to come through")
	}

	// the over-cap message must be marked \Seen, not merely filtered, so it isn't
	// re-probed every poll; only the (peeked) small message stays unseen
	unseen, err := mb.(*imapMailbox).c.UIDSearch(&goimap.SearchCriteria{
		NotFlag: []goimap.Flag{goimap.FlagSeen},
	}, nil).Wait()
	if err != nil {
		t.Fatalf("re-search unseen: %v", err)
	}
	if n := len(unseen.AllUIDs()); n != 1 {
		t.Fatalf("over-cap message left unread: %d unseen, want 1 (only the small message)", n)
	}
}
