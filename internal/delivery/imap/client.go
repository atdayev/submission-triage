package imap

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
)

const (
	imapDialTimeout = 20 * time.Second
	maxProbeWindow  = 500      // max RFC822.SIZE probes per tick
	maxBatchBytes   = 64 << 20 // max raw bytes fetched per poll
)

// fullMessage is BODY.PEEK[]: whole message, no \Seen side effect.
var fullMessage = &goimap.FetchItemBodySection{Peek: true}

// dialClient opens the TLS transport.
var dialClient = func(addr string) (*imapclient.Client, error) {
	return imapclient.DialTLS(addr, &imapclient.Options{
		Dialer: &net.Dialer{Timeout: imapDialTimeout},
	})
}

// filer performs the low-level move/copy operations behind File. The real one
// wraps imapclient; a fake drives the capability-ladder tests.
type filer interface {
	caps() goimap.CapSet
	create(mailbox string) error
	move(uid goimap.UID, mailbox string) error
	copy(uid goimap.UID, mailbox string) error
	markDeleted(uid goimap.UID) error
	uidExpunge(uid goimap.UID) error
}

type imapMailbox struct {
	c            *imapclient.Client
	maxBytes     int64
	log          *logrus.Entry
	stop         chan struct{}
	stopOnce     sync.Once
	filer        filer
	folderPrefix string
	delim        string
	uidValidity  uint32
	warnNoMove   func() // called once per process when the server can't MOVE or UID EXPUNGE
}

// dialIMAP returns a connect-per-tick factory: each poll opens a fresh
// authenticated, mailbox-selected connection.
func dialIMAP(cfg config.IMAPConfig, auth Authenticator, log *logrus.Entry) func(ctx context.Context) (mailbox, error) {
	// process-level: warn at most once if the server can't MOVE or UID EXPUNGE
	var warnOnce sync.Once
	warnNoMove := func() {
		warnOnce.Do(func() {
			log.Warn("imap: server advertises neither MOVE nor UIDPLUS; filed messages will be DUPLICATED (the original stays in the inbox)")
		})
	}
	return func(ctx context.Context) (mailbox, error) {
		c, err := dialClient(net.JoinHostPort(cfg.Host, cfg.Port))
		if err != nil {
			return nil, fmt.Errorf("imap dial: %w", err)
		}
		mb := &imapMailbox{
			c: c, maxBytes: cfg.MaxMessageBytes(), log: log, stop: make(chan struct{}),
			filer: clientFiler{c: c}, folderPrefix: cfg.FolderPrefix, warnNoMove: warnNoMove,
		}
		// close on ctx cancel to unblock an in-flight command at shutdown
		go mb.closeOnCancel(ctx)
		if err := auth(ctx, c); err != nil {
			_ = mb.Close()
			return nil, fmt.Errorf("imap auth: %w", err)
		}
		sel, err := c.Select(cfg.Mailbox, nil).Wait()
		if err != nil {
			_ = mb.Close()
			return nil, fmt.Errorf("imap select %q: %w", cfg.Mailbox, err)
		}
		mb.uidValidity = sel.UIDValidity
		// refresh caps post-auth: MOVE/UIDPLUS are often advertised only after login
		if _, err := c.Capability().Wait(); err != nil {
			log.WithError(err).Debug("imap: capability refresh failed; using cached caps")
		}
		mb.delim = discoverDelim(c)
		return mb, nil
	}
}

// discoverDelim reads the server's hierarchy delimiter from LIST "" "" (`/` on
// Gmail, `\` on some Exchange configs), defaulting to `/` if unavailable.
func discoverDelim(c *imapclient.Client) string {
	data, err := c.List("", "", nil).Collect()
	if err == nil {
		for _, d := range data {
			if d.Delim != 0 {
				return string(d.Delim)
			}
		}
	}
	return "/"
}

func (m *imapMailbox) closeOnCancel(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = m.c.Close()
	case <-m.stop:
	}
}

// FetchUnseen returns unseen messages delivered at or after since, size-filtered
// and capped by limit. The cutoff is applied server-side so a mailbox with years of
// history isn't pulled over the wire only to be discarded.
func (m *imapMailbox) FetchUnseen(ctx context.Context, limit int, since time.Time) ([]rawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	criteria := &goimap.SearchCriteria{NotFlag: []goimap.Flag{goimap.FlagSeen}}
	if !since.IsZero() {
		criteria.Since = since.UTC()
	}
	data, err := m.c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap search: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return nil, nil
	}
	// bound per-tick probe work on a large backlog
	if len(uids) > maxProbeWindow {
		uids = uids[:maxProbeWindow]
	}

	// filter by size before the limit so over-cap mail can't starve the batch
	uids = m.underCap(ctx, uids)
	if len(uids) == 0 {
		return nil, nil
	}
	if limit > 0 && len(uids) > limit {
		uids = uids[:limit]
	}

	msgs, err := m.c.Fetch(goimap.UIDSetNum(uids...), &goimap.FetchOptions{
		UID:         true,
		BodySection: []*goimap.FetchItemBodySection{fullMessage},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap fetch: %w", err)
	}

	out := make([]rawMessage, 0, len(msgs))
	for _, msg := range msgs {
		raw := msg.FindBodySection(fullMessage)
		if raw == nil {
			continue
		}
		out = append(out, rawMessage{UID: uint32(msg.UID), Raw: raw})
	}
	return out, nil
}

// underCap returns the UIDs within maxBytes (probing RFC822.SIZE only); over-cap
// messages are marked seen, and the kept set is bounded by maxBatchBytes.
func (m *imapMailbox) underCap(ctx context.Context, uids []goimap.UID) []goimap.UID {
	if m.maxBytes <= 0 {
		return uids
	}
	sizes, err := m.c.Fetch(goimap.UIDSetNum(uids...), &goimap.FetchOptions{
		UID:        true,
		RFC822Size: true,
	}).Collect()
	if err != nil {
		// size probe failed; let the body fetch try and surface the error
		return uids
	}
	keep := make([]goimap.UID, 0, len(sizes))
	var batchBytes int64
	for _, s := range sizes {
		if s.RFC822Size > m.maxBytes {
			m.log.WithFields(logrus.Fields{
				"uid":  uint32(s.UID),
				"size": s.RFC822Size,
				"max":  m.maxBytes,
			}).Warn("imap: message over size cap; marking read")
			if err := m.MarkSeen(ctx, uint32(s.UID)); err != nil {
				m.log.WithError(err).WithField("uid", uint32(s.UID)).Warn("imap: mark over-cap seen failed")
			}
			continue
		}
		if len(keep) > 0 && batchBytes+s.RFC822Size > maxBatchBytes {
			break // defer the rest to the next poll to bound memory
		}
		batchBytes += s.RFC822Size
		keep = append(keep, s.UID)
	}
	return keep
}

// UIDValidity reports the selected mailbox's UID generation, read at SELECT.
func (m *imapMailbox) UIDValidity() uint32 { return m.uidValidity }

func (m *imapMailbox) MarkSeen(_ context.Context, uid uint32) error {
	return m.c.Store(goimap.UIDSetNum(goimap.UID(uid)), &goimap.StoreFlags{
		Op:     goimap.StoreFlagsAdd,
		Flags:  []goimap.Flag{goimap.FlagSeen},
		Silent: true,
	}, nil).Close()
}

// Flag adds the \Flagged flag, marking the message for human review. Set before
// filing — MOVE invalidates the source UID, so flags must go on the original.
func (m *imapMailbox) Flag(_ context.Context, uid uint32) error {
	return m.c.Store(goimap.UIDSetNum(goimap.UID(uid)), &goimap.StoreFlags{
		Op:     goimap.StoreFlagsAdd,
		Flags:  []goimap.Flag{goimap.FlagFlagged},
		Silent: true,
	}, nil).Close()
}

// File moves the message into its per-status folder, creating the folder on
// first use. The caller marks \Seen first — MOVE removes the source UID, so
// mark-seen must happen before filing.
func (m *imapMailbox) File(_ context.Context, uid uint32, folder string) error {
	return fileByLadder(m.filer, goimap.UID(uid), m.folderPath(folder), m.warnNoMove)
}

// folderPath joins the configured prefix and the status folder with the server's
// hierarchy delimiter (e.g. "Triage/Waiting on Broker").
func (m *imapMailbox) folderPath(leaf string) string {
	if m.folderPrefix == "" {
		return leaf
	}
	return m.folderPrefix + m.delim + leaf
}

// ScanFolder returns the Message-IDs of messages in folder, read-only and capped
// at limit. A missing folder is created (so the agency has somewhere to drag) and
// treated as empty. Selecting the folder ends the INBOX selection, so the poller
// calls this only after the inbox pass is done.
func (m *imapMailbox) ScanFolder(ctx context.Context, folder string, limit int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := m.folderPath(folder)
	sel, err := m.c.Select(path, &goimap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		_ = m.c.Create(path, nil).Wait()
		sel, err = m.c.Select(path, &goimap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return nil, nil // still unavailable; treat as empty
		}
	}
	if sel.NumMessages == 0 {
		return nil, nil
	}
	n := sel.NumMessages
	if limit > 0 && n > uint32(limit) {
		n = uint32(limit)
	}
	nums := make([]uint32, 0, n)
	for i := uint32(1); i <= n; i++ {
		nums = append(nums, i)
	}
	msgs, err := m.c.Fetch(goimap.SeqSetNum(nums...), &goimap.FetchOptions{Envelope: true}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap fetch hold envelopes: %w", err)
	}
	out := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Envelope != nil {
			if id := strings.Trim(msg.Envelope.MessageID, "<>"); id != "" {
				out = append(out, id)
			}
		}
	}
	return out, nil
}

// fileByLadder files uid into folder using the strongest capability the server
// offers: MOVE (RFC 6851); else UIDPLUS copy + flag-deleted + targeted UID
// EXPUNGE (never a bare EXPUNGE, which would remove other \Deleted messages we
// never touched); else a bare COPY that duplicates the message, warned once.
func fileByLadder(f filer, uid goimap.UID, folder string, warnNoMove func()) error {
	switch caps := f.caps(); {
	case caps.Has(goimap.CapMove):
		return withCreate(f, folder, func() error { return f.move(uid, folder) })
	case caps.Has(goimap.CapUIDPlus):
		if err := withCreate(f, folder, func() error { return f.copy(uid, folder) }); err != nil {
			return err
		}
		if err := f.markDeleted(uid); err != nil {
			return fmt.Errorf("imap: flag deleted: %w", err)
		}
		if err := f.uidExpunge(uid); err != nil {
			return fmt.Errorf("imap: uid expunge: %w", err)
		}
		return nil
	default:
		warnNoMove()
		return withCreate(f, folder, func() error { return f.copy(uid, folder) })
	}
}

// withCreate runs op, creating the destination folder and retrying once on failure.
func withCreate(f filer, folder string, op func() error) error {
	if err := op(); err != nil {
		_ = f.create(folder)
		return op()
	}
	return nil
}

// clientFiler is the imapclient-backed filer.
type clientFiler struct{ c *imapclient.Client }

func (f clientFiler) caps() goimap.CapSet         { return f.c.Caps() }
func (f clientFiler) create(mailbox string) error { return f.c.Create(mailbox, nil).Wait() }

func (f clientFiler) move(uid goimap.UID, mailbox string) error {
	_, err := f.c.Move(goimap.UIDSetNum(uid), mailbox).Wait()
	return err
}

func (f clientFiler) copy(uid goimap.UID, mailbox string) error {
	_, err := f.c.Copy(goimap.UIDSetNum(uid), mailbox).Wait()
	return err
}

func (f clientFiler) markDeleted(uid goimap.UID) error {
	return f.c.Store(goimap.UIDSetNum(uid), &goimap.StoreFlags{
		Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagDeleted}, Silent: true,
	}, nil).Close()
}

func (f clientFiler) uidExpunge(uid goimap.UID) error {
	return f.c.UIDExpunge(goimap.UIDSetNum(uid)).Close()
}

// Close logs out and tears down the connection.
func (m *imapMailbox) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	_ = m.c.Logout().Wait()
	return m.c.Close()
}
