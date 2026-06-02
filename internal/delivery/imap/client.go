package imap

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
)

const imapDialTimeout = 20 * time.Second

// fullMessage is BODY.PEEK[]: the whole message, PEEK so the server leaves \Seen alone
var fullMessage = &goimap.FetchItemBodySection{Peek: true}

// dialClient opens the transport; a var so tests can swap in a plaintext dial.
var dialClient = func(addr string) (*imapclient.Client, error) {
	return imapclient.DialTLS(addr, &imapclient.Options{
		Dialer: &net.Dialer{Timeout: imapDialTimeout},
	})
}

type imapMailbox struct {
	c        *imapclient.Client
	maxBytes int64
	log      *logrus.Entry
	stop     chan struct{}
	stopOnce sync.Once
}

// dialIMAP returns a connect-per-tick factory: each poll opens a fresh
// logged-in, mailbox-selected connection.
func dialIMAP(cfg config.IMAPConfig, log *logrus.Entry) func(ctx context.Context) (mailbox, error) {
	return func(ctx context.Context) (mailbox, error) {
		c, err := dialClient(net.JoinHostPort(cfg.Host, cfg.Port))
		if err != nil {
			return nil, fmt.Errorf("imap dial: %w", err)
		}
		mb := &imapMailbox{c: c, maxBytes: cfg.MaxMessageBytes(), log: log, stop: make(chan struct{})}
		// close the connection on ctx cancel so an in-flight command unblocks at shutdown
		go mb.closeOnCancel(ctx)
		if err := c.Login(cfg.Username, cfg.Password).Wait(); err != nil {
			_ = mb.Close()
			return nil, fmt.Errorf("imap login: %w", err)
		}
		if _, err := c.Select(cfg.Mailbox, nil).Wait(); err != nil {
			_ = mb.Close()
			return nil, fmt.Errorf("imap select %q: %w", cfg.Mailbox, err)
		}
		return mb, nil
	}
}

func (m *imapMailbox) closeOnCancel(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = m.c.Close()
	case <-m.stop:
	}
}

func (m *imapMailbox) FetchUnseen(ctx context.Context, limit int) ([]rawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := m.c.UIDSearch(&goimap.SearchCriteria{
		NotFlag: []goimap.Flag{goimap.FlagSeen},
	}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap search: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return nil, nil
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

// underCap returns the UIDs whose server-reported size is within maxBytes,
// fetching only RFC822.SIZE (no bodies). A zero cap keeps everything. Over-cap
// messages are marked seen so they don't recur every poll.
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
	for _, s := range sizes {
		if s.RFC822Size > m.maxBytes {
			m.log.WithFields(logrus.Fields{
				"uid":  uint32(s.UID),
				"size": s.RFC822Size,
				"max":  m.maxBytes,
			}).Warn("imap: message over size cap; marking seen")
			if err := m.MarkSeen(ctx, uint32(s.UID)); err != nil {
				m.log.WithError(err).WithField("uid", uint32(s.UID)).Warn("imap: mark over-cap seen failed")
			}
			continue
		}
		keep = append(keep, s.UID)
	}
	return keep
}

func (m *imapMailbox) MarkSeen(_ context.Context, uid uint32) error {
	return m.c.Store(goimap.UIDSetNum(goimap.UID(uid)), &goimap.StoreFlags{
		Op:     goimap.StoreFlagsAdd,
		Flags:  []goimap.Flag{goimap.FlagSeen},
		Silent: true,
	}, nil).Close()
}

func (m *imapMailbox) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	_ = m.c.Logout().Wait()
	return m.c.Close()
}
