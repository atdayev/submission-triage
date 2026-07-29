package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/model"
)

//go:generate mockery --name=SubmissionRepository --output=mocks --outpkg=mocks --filename=SubmissionRepository.go

// SubmissionRepository persists submissions and their emails.
type SubmissionRepository interface {
	UpsertSubmission(ctx context.Context, s *model.Submission) error
	// UpsertSubmissionWithReply persists the submission and its reply in one tx.
	UpsertSubmissionWithReply(ctx context.Context, s *model.Submission, reply *model.OutboxEntry) error
	FindByEmailReference(ctx context.Context, messageIDs []string) (*model.Submission, bool, error)
	FindByDeterministicID(ctx context.Context, deterministicID string) (*model.Submission, error)
	ListStale(ctx context.Context, olderThanUnixNano int64, limit int) ([]model.Submission, error)
	// ListBindSoon returns awaiting submissions whose effective date falls inside the
	// bind window, excluding held/delivery-failed ones.
	ListBindSoon(ctx context.Context, effectiveAfterUnixNano, effectiveBeforeUnixNano int64, limit int) ([]model.Submission, error)
	ListCompletedBefore(ctx context.Context, olderThanUnixNano int64, limit int) ([]model.Submission, error)
	ListEscalatedSince(ctx context.Context, sinceUnixNano int64, limit int) ([]model.Submission, error)
	// ListStalledBefore returns awaiting and open submissions idle since the cutoff —
	// the ones no other sweep reaches, so without it they never terminate.
	ListStalledBefore(ctx context.Context, olderThanUnixNano int64, limit int) ([]model.Submission, error)
	// ClearExtractedTextForClosed drops the stored document text of submissions closed
	// before the cutoff; it is the bulk of what a submission occupies on disk.
	ClearExtractedTextForClosed(ctx context.Context, closedBeforeUnixNano int64) (int64, error)
	// ListOpen returns non-closed submissions for the daily digest, ordered by
	// state then oldest inactivity, capped at limit.
	ListOpen(ctx context.Context, limit int) ([]model.Submission, error)
	// CountOpen returns the total non-closed submission count (for the digest's
	// omitted line when the cap is hit).
	CountOpen(ctx context.Context) (int, error)
	// FilenameOnlyItems returns, per submission id, the item ids satisfied by a
	// filename glob alone (no keyword/LLM corroboration) — the digest flags these.
	FilenameOnlyItems(ctx context.Context, submissionIDs []string) (map[string][]string, error)
	UpsertEmail(ctx context.Context, e *model.Email) error
	// SetLastReplyAt records when a submission's last reply was sent (coalesce spacing).
	SetLastReplyAt(ctx context.Context, submissionID string, t time.Time) error
	// ClearReplyHash forgets the last reply's fingerprint, so the same ask is allowed
	// to be made again. Used when a queued reply dead-letters: the broker never
	// received it, so suppressing the repeat would strand the submission.
	ClearReplyHash(ctx context.Context, submissionID string) error
	// SetOnHold sets on_hold on the given submissions (declarative Hold-folder sync).
	SetOnHold(ctx context.Context, submissionIDs []string, onHold bool) error
	// ListHeldIDs returns the ids of all currently-held submissions.
	ListHeldIDs(ctx context.Context) ([]string, error)
	// ReplyBlockedIDs returns which of the given submissions must not be replied to
	// right now — held or hard-bounced. Checked at dispatch, not only at build:
	// either can become true after a reply is queued but before its window elapses.
	ReplyBlockedIDs(ctx context.Context, submissionIDs []string) ([]string, error)
	// SubmissionIDsByMessageIDs resolves inbound Message-IDs to their submission ids.
	SubmissionIDsByMessageIDs(ctx context.Context, messageIDs []string) ([]string, error)
	// FindContentMatch returns the most recent open/awaiting submission from the same
	// sender with the same normalized named insured, created since the cutoff.
	FindContentMatch(ctx context.Context, namedInsured, fromAddress string, createdAfterUnixNano int64) (*model.Submission, error)
}

type scanner interface {
	Scan(dest ...any) error
}

type SubmissionRepositoryImpl struct {
	db  *sql.DB
	log *logrus.Entry
}

func NewSubmissionRepository(db *sql.DB, log *logrus.Entry) *SubmissionRepositoryImpl {
	return &SubmissionRepositoryImpl{db: db, log: log}
}

// UpsertSubmission persists a submission with its emails and documents in one tx.
func (r *SubmissionRepositoryImpl) UpsertSubmission(ctx context.Context, s *model.Submission) error {
	if s == nil || s.ID == "" {
		return errors.New("sqlite: UpsertSubmission requires non-empty id")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := upsertSubmissionRow(ctx, tx, s); err != nil {
		return err
	}
	for i := range s.Emails {
		if err := upsertEmailRow(ctx, tx, &s.Emails[i]); err != nil && !errors.Is(err, model.ErrDuplicateEmail) {
			return err
		}
	}
	for i := range s.Documents {
		if err := upsertDocumentRow(ctx, tx, &s.Documents[i]); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// UpsertSubmissionWithReply persists the submission and its reply in one tx.
func (r *SubmissionRepositoryImpl) UpsertSubmissionWithReply(ctx context.Context, s *model.Submission, reply *model.OutboxEntry) error {
	if s == nil || s.ID == "" {
		return errors.New("sqlite: UpsertSubmissionWithReply requires non-empty id")
	}
	if reply == nil {
		return errors.New("sqlite: UpsertSubmissionWithReply requires non-nil reply")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := upsertSubmissionRow(ctx, tx, s); err != nil {
		return err
	}
	for i := range s.Emails {
		if err := upsertEmailRow(ctx, tx, &s.Emails[i]); err != nil && !errors.Is(err, model.ErrDuplicateEmail) {
			return err
		}
	}
	for i := range s.Documents {
		if err := upsertDocumentRow(ctx, tx, &s.Documents[i]); err != nil {
			return err
		}
	}
	if err := insertOutboxRow(ctx, tx, reply); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// GetByID loads a submission with its emails and documents.
func (r *SubmissionRepositoryImpl) GetByID(ctx context.Context, id string) (*model.Submission, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, policy_type, state, subject_line, from_address, from_name,
		       thread_key, created_at, updated_at, last_action_at,
		       escalated_at, missing_items, last_reply_at, needs_review,
		       named_insured, effective_date, on_hold, delivery_failed, last_escalated_at,
		       completed_at, last_reply_hash, policy_clarify_asks
		FROM submissions WHERE id = ?`, id)

	s, err := scanSubmission(row, r.log)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrSubmissionNotFound
		}
		return nil, fmt.Errorf("scan submission: %w", err)
	}

	emails, err := loadEmails(ctx, r.db, id, r.log)
	if err != nil {
		return nil, err
	}
	s.Emails = emails

	docs, err := loadDocuments(ctx, r.db, id)
	if err != nil {
		return nil, err
	}
	s.Documents = docs
	return s, nil
}

// FindByEmailReference finds the submission threaded to any of the given Message-IDs.
func (r *SubmissionRepositoryImpl) FindByEmailReference(ctx context.Context, messageIDs []string) (*model.Submission, bool, error) {
	refs := nonEmpty(messageIDs)
	if len(refs) == 0 {
		return nil, false, model.ErrSubmissionNotFound
	}

	matches, err := r.findSubmissionIDsByRefs(ctx, refs)
	if err != nil {
		return nil, false, err
	}
	if len(matches) == 0 {
		return nil, false, model.ErrSubmissionNotFound
	}

	bestID := matches[0]
	ambiguous := len(matches) > 1
	if ambiguous {
		bestID, err = r.pickMostRecentlyUpdated(ctx, matches)
		if err != nil {
			return nil, false, err
		}
	}

	s, err := r.GetByID(ctx, bestID)
	if err != nil {
		return nil, false, err
	}
	return s, ambiguous, nil
}

// FindByDeterministicID finds the submission owning the email with the given deterministic ID.
func (r *SubmissionRepositoryImpl) FindByDeterministicID(ctx context.Context, deterministicID string) (*model.Submission, error) {
	if deterministicID == "" {
		return nil, model.ErrSubmissionNotFound
	}
	var submissionID string
	err := r.db.QueryRowContext(ctx,
		`SELECT submission_id FROM emails WHERE deterministic_id = ?`, deterministicID).Scan(&submissionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.ErrSubmissionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query deterministic id: %w", err)
	}
	return r.GetByID(ctx, submissionID)
}

func (r *SubmissionRepositoryImpl) findSubmissionIDsByRefs(ctx context.Context, refs []string) ([]string, error) {
	in := placeholderList(len(refs))
	args := make([]any, 0, len(refs)*2)
	for _, ref := range refs {
		args = append(args, ref)
	}
	for _, ref := range refs {
		args = append(args, ref)
	}
	query := fmt.Sprintf(`
		SELECT DISTINCT submission_id FROM emails
		WHERE message_id IN (%s) OR in_reply_to IN (%s)`, in, in)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query thread match: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan thread match: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *SubmissionRepositoryImpl) pickMostRecentlyUpdated(ctx context.Context, ids []string) (string, error) {
	in := placeholderList(len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	// id breaks updated_at ties
	query := fmt.Sprintf(`SELECT id FROM submissions WHERE id IN (%s) ORDER BY updated_at DESC, id DESC LIMIT 1`, in)
	var best string
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&best); err != nil {
		return "", fmt.Errorf("select most-recent thread: %w", err)
	}
	return best, nil
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// nanoOrNow returns t as UnixNano, substituting now for a zero time (UnixNano of zero is year 1754).
func nanoOrNow(t time.Time) int64 {
	if t.IsZero() {
		return time.Now().UTC().UnixNano()
	}
	return t.UnixNano()
}

// nanoOrZero returns t as UnixNano, or 0 for a zero time (used where 0 is a
// meaningful "never" sentinel, e.g. last_reply_at).
func nanoOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func placeholderList(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

// ListCompletedBefore returns submissions that completed before the given time.
// Keyed on completed_at, not updated_at: inbound mail moves updated_at, so a broker
// thanking us every day would otherwise defer auto-close indefinitely.
func (r *SubmissionRepositoryImpl) ListCompletedBefore(ctx context.Context, olderThanUnixNano int64, limit int) ([]model.Submission, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, policy_type, state, subject_line, from_address, from_name,
		       thread_key, created_at, updated_at, last_action_at,
		       escalated_at, missing_items, last_reply_at, needs_review,
		       named_insured, effective_date, on_hold, delivery_failed, last_escalated_at,
		       completed_at, last_reply_hash, policy_clarify_asks
		FROM submissions
		WHERE state = ? AND completed_at IS NOT NULL AND completed_at < ? AND on_hold = 0
		ORDER BY completed_at ASC
		LIMIT ?`, string(model.StateComplete), olderThanUnixNano, limit)
	if err != nil {
		return nil, fmt.Errorf("query completed-before: %w", err)
	}
	defer rows.Close()

	var out []model.Submission
	for rows.Next() {
		s, err := scanSubmission(rows, r.log)
		if err != nil {
			return nil, fmt.Errorf("scan completed-before: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ListEscalatedSince returns escalated submissions escalated at or after the given time.
func (r *SubmissionRepositoryImpl) ListEscalatedSince(ctx context.Context, sinceUnixNano int64, limit int) ([]model.Submission, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, policy_type, state, subject_line, from_address, from_name,
		       thread_key, created_at, updated_at, last_action_at,
		       escalated_at, missing_items, last_reply_at, needs_review,
		       named_insured, effective_date, on_hold, delivery_failed, last_escalated_at,
		       completed_at, last_reply_hash, policy_clarify_asks
		FROM submissions
		WHERE state = ? AND escalated_at IS NOT NULL AND escalated_at >= ? AND on_hold = 0
		ORDER BY escalated_at ASC
		LIMIT ?`, string(model.StateEscalated), sinceUnixNano, limit)
	if err != nil {
		return nil, fmt.Errorf("query escalated-since: %w", err)
	}
	defer rows.Close()

	var out []model.Submission
	for rows.Next() {
		s, err := scanSubmission(rows, r.log)
		if err != nil {
			return nil, fmt.Errorf("scan escalated-since: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ListStale returns awaiting submissions with no action since the given time.
func (r *SubmissionRepositoryImpl) ListStale(ctx context.Context, olderThanUnixNano int64, limit int) ([]model.Submission, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, policy_type, state, subject_line, from_address, from_name,
		       thread_key, created_at, updated_at, last_action_at,
		       escalated_at, missing_items, last_reply_at, needs_review,
		       named_insured, effective_date, on_hold, delivery_failed, last_escalated_at,
		       completed_at, last_reply_hash, policy_clarify_asks
		FROM submissions
		WHERE state = ? AND last_action_at < ? AND on_hold = 0 AND delivery_failed = 0
		ORDER BY last_action_at ASC
		LIMIT ?`, string(model.StateAwaiting), olderThanUnixNano, limit)
	if err != nil {
		return nil, fmt.Errorf("query stale: %w", err)
	}
	defer rows.Close()

	var out []model.Submission
	for rows.Next() {
		s, err := scanSubmission(rows, r.log)
		if err != nil {
			return nil, fmt.Errorf("scan stale: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ListBindSoon returns awaiting submissions binding within the window. The lower
// bound retires a submission once its effective date is well past — without it a
// bound-and-gone case re-nudges every day forever.
func (r *SubmissionRepositoryImpl) ListBindSoon(ctx context.Context, effectiveAfterUnixNano, effectiveBeforeUnixNano int64, limit int) ([]model.Submission, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, policy_type, state, subject_line, from_address, from_name,
		       thread_key, created_at, updated_at, last_action_at,
		       escalated_at, missing_items, last_reply_at, needs_review,
		       named_insured, effective_date, on_hold, delivery_failed, last_escalated_at,
		       completed_at, last_reply_hash, policy_clarify_asks
		FROM submissions
		WHERE state = ? AND effective_date IS NOT NULL
		  AND effective_date >= ? AND effective_date <= ?
		  AND on_hold = 0 AND delivery_failed = 0
		ORDER BY effective_date ASC
		LIMIT ?`, string(model.StateAwaiting), effectiveAfterUnixNano, effectiveBeforeUnixNano, limit)
	if err != nil {
		return nil, fmt.Errorf("query bind-soon: %w", err)
	}
	defer rows.Close()

	var out []model.Submission
	for rows.Next() {
		s, err := scanSubmission(rows, r.log)
		if err != nil {
			return nil, fmt.Errorf("scan bind-soon: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ListStalledBefore returns awaiting and open submissions idle since the cutoff.
func (r *SubmissionRepositoryImpl) ListStalledBefore(ctx context.Context, olderThanUnixNano int64, limit int) ([]model.Submission, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, policy_type, state, subject_line, from_address, from_name,
		       thread_key, created_at, updated_at, last_action_at,
		       escalated_at, missing_items, last_reply_at, needs_review,
		       named_insured, effective_date, on_hold, delivery_failed, last_escalated_at,
		       completed_at, last_reply_hash, policy_clarify_asks
		FROM submissions
		WHERE state IN (?, ?) AND last_action_at < ? AND on_hold = 0
		ORDER BY last_action_at ASC
		LIMIT ?`, string(model.StateAwaiting), string(model.StateOpen), olderThanUnixNano, limit)
	if err != nil {
		return nil, fmt.Errorf("query stalled-before: %w", err)
	}
	defer rows.Close()

	var out []model.Submission
	for rows.Next() {
		s, err := scanSubmission(rows, r.log)
		if err != nil {
			return nil, fmt.Errorf("scan stalled-before: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ClearExtractedTextForClosed nulls out the stored text of closed submissions'
// documents. The classification verdict, filename, and hash all stay — only the
// text body goes, and only once the case can no longer be reopened by a sweep.
func (r *SubmissionRepositoryImpl) ClearExtractedTextForClosed(ctx context.Context, closedBeforeUnixNano int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE documents SET extracted_text = ''
		WHERE extracted_text != '' AND submission_id IN (
			SELECT id FROM submissions WHERE state = ? AND updated_at < ?
		)`, string(model.StateClosed), closedBeforeUnixNano)
	if err != nil {
		return 0, fmt.Errorf("clear extracted text: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListOpen returns non-closed submissions, ordered by state then oldest inactivity.
func (r *SubmissionRepositoryImpl) ListOpen(ctx context.Context, limit int) ([]model.Submission, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, policy_type, state, subject_line, from_address, from_name,
		       thread_key, created_at, updated_at, last_action_at,
		       escalated_at, missing_items, last_reply_at, needs_review,
		       named_insured, effective_date, on_hold, delivery_failed, last_escalated_at,
		       completed_at, last_reply_hash, policy_clarify_asks
		FROM submissions
		WHERE state != ?
		ORDER BY state ASC, last_action_at ASC
		LIMIT ?`, string(model.StateClosed), limit)
	if err != nil {
		return nil, fmt.Errorf("query open: %w", err)
	}
	defer rows.Close()

	var out []model.Submission
	for rows.Next() {
		s, err := scanSubmission(rows, r.log)
		if err != nil {
			return nil, fmt.Errorf("scan open: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// FilenameOnlyItems returns, per submission id, the item ids a filename glob
// satisfied with no keyword/LLM corroboration. Submissions with none are absent.
func (r *SubmissionRepositoryImpl) FilenameOnlyItems(ctx context.Context, submissionIDs []string) (map[string][]string, error) {
	if len(submissionIDs) == 0 {
		return nil, nil
	}
	in := placeholderList(len(submissionIDs))
	args := make([]any, len(submissionIDs))
	for i, id := range submissionIDs {
		args[i] = id
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT submission_id, classified_as, classified_by
		FROM documents
		WHERE submission_id IN (%s) AND classified_as != ''`, in), args...)
	if err != nil {
		return nil, fmt.Errorf("query filename-only: %w", err)
	}
	defer rows.Close()

	docsBySub := make(map[string][]model.Document)
	for rows.Next() {
		var sid, classifiedAs, classifiedBy string
		if err := rows.Scan(&sid, &classifiedAs, &classifiedBy); err != nil {
			return nil, fmt.Errorf("scan filename-only: %w", err)
		}
		docsBySub[sid] = append(docsBySub[sid], model.Document{ClassifiedAs: classifiedAs, ClassifiedBy: classifiedBy})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(docsBySub))
	for sid, docs := range docsBySub {
		if ids := model.FilenameOnlyMatches(docs); len(ids) > 0 {
			out[sid] = ids
		}
	}
	return out, nil
}

// CountOpen returns the number of non-closed submissions.
func (r *SubmissionRepositoryImpl) CountOpen(ctx context.Context) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM submissions WHERE state != ?`, string(model.StateClosed)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count open: %w", err)
	}
	return n, nil
}

// SetLastReplyAt records when a submission's last reply was sent.
func (r *SubmissionRepositoryImpl) SetLastReplyAt(ctx context.Context, submissionID string, t time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE submissions SET last_reply_at = ? WHERE id = ?`, nanoOrZero(t), submissionID)
	if err != nil {
		return fmt.Errorf("set last_reply_at: %w", err)
	}
	return nil
}

// ClearReplyHash forgets the last reply's fingerprint so the same ask may repeat.
func (r *SubmissionRepositoryImpl) ClearReplyHash(ctx context.Context, submissionID string) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE submissions SET last_reply_hash = '' WHERE id = ?`, submissionID); err != nil {
		return fmt.Errorf("clear reply hash: %w", err)
	}
	return nil
}

// SetOnHold sets on_hold for the given submissions in one statement (no-op on empty).
func (r *SubmissionRepositoryImpl) SetOnHold(ctx context.Context, submissionIDs []string, onHold bool) error {
	if len(submissionIDs) == 0 {
		return nil
	}
	in := placeholderList(len(submissionIDs))
	args := make([]any, 0, len(submissionIDs)+1)
	args = append(args, boolToInt(onHold))
	for _, id := range submissionIDs {
		args = append(args, id)
	}
	if _, err := r.db.ExecContext(ctx, fmt.Sprintf(`UPDATE submissions SET on_hold = ? WHERE id IN (%s)`, in), args...); err != nil {
		return fmt.Errorf("set on_hold: %w", err)
	}
	return nil
}

// ListHeldIDs returns the ids of all currently-held submissions.
func (r *SubmissionRepositoryImpl) ListHeldIDs(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM submissions WHERE on_hold = 1`)
	if err != nil {
		return nil, fmt.Errorf("list held: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows, "held")
}

// ReplyBlockedIDs returns which of the given submissions are held or hard-bounced.
func (r *SubmissionRepositoryImpl) ReplyBlockedIDs(ctx context.Context, submissionIDs []string) ([]string, error) {
	ids := nonEmpty(submissionIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	in := placeholderList(len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT id FROM submissions WHERE id IN (%s) AND (on_hold = 1 OR delivery_failed = 1)`, in), args...)
	if err != nil {
		return nil, fmt.Errorf("query reply-blocked: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows, "reply-blocked id")
}

// SubmissionIDsByMessageIDs resolves inbound Message-IDs to distinct submission ids.
func (r *SubmissionRepositoryImpl) SubmissionIDsByMessageIDs(ctx context.Context, messageIDs []string) ([]string, error) {
	ids := nonEmpty(messageIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	in := placeholderList(len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`SELECT DISTINCT submission_id FROM emails WHERE message_id IN (%s)`, in), args...)
	if err != nil {
		return nil, fmt.Errorf("query submissions by message id: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows, "submission id")
}

// FindContentMatch returns the most recent open/awaiting submission from the same
// sender with the same normalized named insured, created at or after the cutoff.
func (r *SubmissionRepositoryImpl) FindContentMatch(ctx context.Context, namedInsured, fromAddress string, createdAfterUnixNano int64) (*model.Submission, error) {
	insured := strings.ToLower(strings.TrimSpace(namedInsured))
	if insured == "" || fromAddress == "" {
		return nil, model.ErrSubmissionNotFound
	}
	var id string
	err := r.db.QueryRowContext(ctx, `
		SELECT id FROM submissions
		WHERE named_insured != '' AND lower(trim(named_insured)) = ? AND from_address = ?
		  AND state IN (?, ?) AND created_at >= ?
		ORDER BY created_at DESC LIMIT 1`,
		insured, fromAddress, string(model.StateOpen), string(model.StateAwaiting), createdAfterUnixNano).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.ErrSubmissionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("content match: %w", err)
	}
	return r.GetByID(ctx, id)
}

// scanIDs collects a single-column id result set.
func scanIDs(rows *sql.Rows, what string) ([]string, error) {
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan %s: %w", what, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UpsertEmail persists a single email in its own tx.
func (r *SubmissionRepositoryImpl) UpsertEmail(ctx context.Context, e *model.Email) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertEmailRow(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertSubmissionRow(ctx context.Context, tx *sql.Tx, s *model.Submission) error {
	missingJSON, err := json.Marshal(s.MissingItems)
	if err != nil {
		return fmt.Errorf("marshal missing items: %w", err)
	}
	if string(missingJSON) == "null" {
		missingJSON = []byte("[]")
	}
	var escalatedAt sql.NullInt64
	if s.EscalatedAt != nil {
		escalatedAt = sql.NullInt64{Int64: s.EscalatedAt.UnixNano(), Valid: true}
	}
	var effectiveDate sql.NullInt64
	if s.EffectiveDate != nil {
		effectiveDate = sql.NullInt64{Int64: s.EffectiveDate.UnixNano(), Valid: true}
	}
	var lastEscalated sql.NullInt64
	if s.LastEscalatedAt != nil {
		lastEscalated = sql.NullInt64{Int64: s.LastEscalatedAt.UnixNano(), Valid: true}
	}
	var completedAt sql.NullInt64
	if s.CompletedAt != nil {
		completedAt = sql.NullInt64{Int64: s.CompletedAt.UnixNano(), Valid: true}
	}
	// last_reply_at and on_hold are written on insert but never on conflict:
	// SetLastReplyAt and the Hold-folder reconcile own them, so a re-upsert can't
	// clobber a concurrent send's timestamp or a human's hold toggle.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO submissions (
			id, policy_type, state, subject_line, from_address, from_name,
			thread_key, created_at, updated_at, last_action_at,
			escalated_at, missing_items, last_reply_at, needs_review,
			named_insured, effective_date, on_hold, delivery_failed, last_escalated_at,
			completed_at, last_reply_hash, policy_clarify_asks
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			policy_type=excluded.policy_type,
			state=excluded.state,
			subject_line=excluded.subject_line,
			from_address=excluded.from_address,
			from_name=excluded.from_name,
			thread_key=excluded.thread_key,
			updated_at=excluded.updated_at,
			last_action_at=excluded.last_action_at,
			escalated_at=excluded.escalated_at,
			missing_items=excluded.missing_items,
			needs_review=excluded.needs_review,
			named_insured=excluded.named_insured,
			effective_date=excluded.effective_date,
			delivery_failed=excluded.delivery_failed,
			last_escalated_at=excluded.last_escalated_at,
			completed_at=excluded.completed_at,
			last_reply_hash=excluded.last_reply_hash,
			policy_clarify_asks=excluded.policy_clarify_asks`,
		s.ID, s.PolicyType, string(s.State), s.SubjectLine, s.FromAddress, s.FromName,
		s.ThreadKey, nanoOrNow(s.CreatedAt), nanoOrNow(s.UpdatedAt), nanoOrNow(s.LastActionAt),
		escalatedAt, string(missingJSON), nanoOrZero(s.LastReplyAt), boolToInt(s.NeedsReview),
		s.NamedInsured, effectiveDate, boolToInt(s.OnHold), boolToInt(s.DeliveryFailed), lastEscalated,
		completedAt, s.LastReplyHash, s.PolicyClarifyAsks,
	)
	if err != nil {
		return fmt.Errorf("upsert submission: %w", err)
	}
	return nil
}

func upsertEmailRow(ctx context.Context, tx *sql.Tx, e *model.Email) error {
	refsJSON, err := json.Marshal(e.References)
	if err != nil {
		return fmt.Errorf("marshal references: %w", err)
	}
	toJSON, err := json.Marshal(e.ToAddresses)
	if err != nil {
		return fmt.Errorf("marshal to addresses: %w", err)
	}
	attachJSON, err := json.Marshal(stripAttachmentContent(e.Attachments))
	if err != nil {
		return fmt.Errorf("marshal attachments: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO emails (
			deterministic_id, submission_id, direction, message_id, in_reply_to,
			refs, from_address, from_name, to_addresses, subject,
			body_text, received_at, provider_msg_id, attachments, reply_to
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(deterministic_id) DO NOTHING`,
		e.DeterministicID, e.SubmissionID, string(e.Direction), e.MessageID, e.InReplyTo,
		string(refsJSON), e.FromAddress, e.FromName, string(toJSON), e.Subject,
		e.BodyText, e.ReceivedAt.UnixNano(), e.ProviderMsgID, string(attachJSON), e.ReplyTo,
	)
	if err != nil {
		return fmt.Errorf("upsert email: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return model.ErrDuplicateEmail
	}
	return nil
}

func upsertDocumentRow(ctx context.Context, tx *sql.Tx, d *model.Document) error {
	if d.ID == "" {
		return errors.New("upsert document: empty id")
	}
	fieldsJSON, err := json.Marshal(d.ExtractedFields)
	if err != nil {
		return fmt.Errorf("marshal extracted_fields: %w", err)
	}
	if string(fieldsJSON) == "null" {
		fieldsJSON = []byte("{}")
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO documents (
			id, submission_id, email_id, filename, content_type, size_bytes,
			sha256, classified_as, confidence, classified_by,
			extracted_text, extracted_fields, unreadable, encrypted, created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			classified_as=excluded.classified_as,
			confidence=excluded.confidence,
			classified_by=excluded.classified_by,
			extracted_text=excluded.extracted_text,
			extracted_fields=excluded.extracted_fields,
			unreadable=excluded.unreadable,
			encrypted=excluded.encrypted`,
		d.ID, d.SubmissionID, d.EmailID, d.Filename, d.ContentType, d.SizeBytes,
		d.SHA256, d.ClassifiedAs, d.Confidence, d.ClassifiedBy,
		d.ExtractedText, string(fieldsJSON), boolToInt(d.Unreadable), boolToInt(d.Encrypted), d.CreatedAt.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("upsert document: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func stripAttachmentContent(in []model.Attachment) []model.Attachment {
	out := make([]model.Attachment, len(in))
	for i, a := range in {
		a.Content = nil
		out[i] = a
	}
	return out
}

func scanSubmission(s scanner, log *logrus.Entry) (*model.Submission, error) {
	var (
		sub            model.Submission
		stateStr       string
		createdAt      int64
		updatedAt      int64
		lastAction     int64
		lastReply      int64
		escalatedAt    sql.NullInt64
		missingJSON    string
		needsReview    int
		effectiveDate  sql.NullInt64
		onHold         int
		deliveryFailed int
		lastEscalated  sql.NullInt64
		completedAt    sql.NullInt64
	)
	err := s.Scan(
		&sub.ID, &sub.PolicyType, &stateStr, &sub.SubjectLine, &sub.FromAddress, &sub.FromName,
		&sub.ThreadKey, &createdAt, &updatedAt, &lastAction,
		&escalatedAt, &missingJSON, &lastReply, &needsReview,
		&sub.NamedInsured, &effectiveDate, &onHold, &deliveryFailed, &lastEscalated,
		&completedAt, &sub.LastReplyHash, &sub.PolicyClarifyAsks,
	)
	if err != nil {
		return nil, err
	}
	sub.State = model.State(stateStr)
	sub.NeedsReview = needsReview != 0
	sub.OnHold = onHold != 0
	sub.DeliveryFailed = deliveryFailed != 0
	if effectiveDate.Valid {
		t := time.Unix(0, effectiveDate.Int64).UTC()
		sub.EffectiveDate = &t
	}
	if lastEscalated.Valid {
		t := time.Unix(0, lastEscalated.Int64).UTC()
		sub.LastEscalatedAt = &t
	}
	if completedAt.Valid {
		t := time.Unix(0, completedAt.Int64).UTC()
		sub.CompletedAt = &t
	}
	sub.CreatedAt = time.Unix(0, createdAt).UTC()
	sub.UpdatedAt = time.Unix(0, updatedAt).UTC()
	sub.LastActionAt = time.Unix(0, lastAction).UTC()
	if lastReply != 0 {
		sub.LastReplyAt = time.Unix(0, lastReply).UTC()
	}
	if escalatedAt.Valid {
		t := time.Unix(0, escalatedAt.Int64).UTC()
		sub.EscalatedAt = &t
	}
	if missingJSON != "" {
		sub.MissingItems = decodeMissingItems(missingJSON, sub.ID, log)
	}
	return &sub, nil
}

// decodeMissingItems accepts both the current []MissingItem and the legacy []string shapes.
func decodeMissingItems(raw, submissionID string, log *logrus.Entry) []model.MissingItem {
	var items []model.MissingItem
	if err := json.Unmarshal([]byte(raw), &items); err == nil {
		return items
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		if log != nil {
			log.WithError(err).WithField("submission_id", submissionID).Warn("missing_items json unmarshal failed")
		}
		return nil
	}
	out := make([]model.MissingItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, model.MissingItem{ID: id})
	}
	return out
}

func loadEmails(ctx context.Context, db *sql.DB, submissionID string, log *logrus.Entry) ([]model.Email, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT deterministic_id, submission_id, direction, message_id, in_reply_to,
		       refs, from_address, from_name, to_addresses, subject,
		       body_text, received_at, provider_msg_id, attachments, reply_to
		FROM emails WHERE submission_id = ?
		ORDER BY received_at ASC`, submissionID)
	if err != nil {
		return nil, fmt.Errorf("query emails: %w", err)
	}
	defer rows.Close()

	var out []model.Email
	for rows.Next() {
		var (
			e               model.Email
			dir             string
			refsJSON        string
			toJSON          string
			receivedAt      int64
			attachmentsJSON string
		)
		if err := rows.Scan(
			&e.DeterministicID, &e.SubmissionID, &dir, &e.MessageID, &e.InReplyTo,
			&refsJSON, &e.FromAddress, &e.FromName, &toJSON, &e.Subject,
			&e.BodyText, &receivedAt, &e.ProviderMsgID, &attachmentsJSON, &e.ReplyTo,
		); err != nil {
			return nil, fmt.Errorf("scan email: %w", err)
		}
		e.Direction = model.Direction(dir)
		e.ReceivedAt = time.Unix(0, receivedAt).UTC()
		if err := json.Unmarshal([]byte(refsJSON), &e.References); err != nil && log != nil {
			log.WithError(err).WithField("email_id", e.DeterministicID).Warn("email refs json unmarshal failed")
		}
		if err := json.Unmarshal([]byte(toJSON), &e.ToAddresses); err != nil && log != nil {
			log.WithError(err).WithField("email_id", e.DeterministicID).Warn("email to_addresses json unmarshal failed")
		}
		if err := json.Unmarshal([]byte(attachmentsJSON), &e.Attachments); err != nil && log != nil {
			log.WithError(err).WithField("email_id", e.DeterministicID).Warn("email attachments json unmarshal failed")
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func loadDocuments(ctx context.Context, db *sql.DB, submissionID string) ([]model.Document, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, submission_id, email_id, filename, content_type, size_bytes,
		       sha256, classified_as, confidence, classified_by,
		       extracted_text, extracted_fields, unreadable, encrypted, created_at
		FROM documents WHERE submission_id = ?
		ORDER BY created_at ASC`, submissionID)
	if err != nil {
		return nil, fmt.Errorf("query documents: %w", err)
	}
	defer rows.Close()

	var out []model.Document
	for rows.Next() {
		var (
			d          model.Document
			fieldsJSON string
			unreadable int
			encrypted  int
			createdAt  int64
		)
		if err := rows.Scan(
			&d.ID, &d.SubmissionID, &d.EmailID, &d.Filename, &d.ContentType, &d.SizeBytes,
			&d.SHA256, &d.ClassifiedAs, &d.Confidence, &d.ClassifiedBy,
			&d.ExtractedText, &fieldsJSON, &unreadable, &encrypted, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		d.Unreadable = unreadable != 0
		d.Encrypted = encrypted != 0
		d.CreatedAt = time.Unix(0, createdAt).UTC()
		if fieldsJSON != "" && fieldsJSON != "{}" {
			if err := json.Unmarshal([]byte(fieldsJSON), &d.ExtractedFields); err != nil {
				d.ExtractedFields = nil
			}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
