package model

import (
	"errors"
	"fmt"
	"time"
)

// State is a submission's lifecycle state.
type State string

const (
	StateOpen      State = "open"
	StateAwaiting  State = "awaiting"
	StateComplete  State = "complete"
	StateEscalated State = "escalated"
	StateClosed    State = "closed"
)

// PolicyTypeUnknown means no checklist matched; gets a clarification reply.
const PolicyTypeUnknown = "unknown"

var (
	ErrInvalidTransition  = errors.New("invalid state transition")
	ErrSubmissionNotFound = errors.New("submission not found")
	ErrDuplicateEmail     = errors.New("duplicate email")
)

var allowed = map[State]map[State]struct{}{
	StateOpen: {
		StateAwaiting:  {},
		StateComplete:  {},
		StateEscalated: {},
		StateClosed:    {},
	},
	StateAwaiting: {
		StateAwaiting:  {},
		StateComplete:  {},
		StateEscalated: {},
		StateClosed:    {},
	},
	StateComplete: {
		StateAwaiting: {},
		StateClosed:   {},
	},
	StateEscalated: {
		StateAwaiting: {},
		StateComplete: {},
		StateClosed:   {},
	},
}

// Submission tracks one incoming submission through triage.
type Submission struct {
	ID           string
	PolicyType   string
	State        State
	SubjectLine  string
	FromAddress  string
	FromName     string
	ThreadKey    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastActionAt time.Time
	LastReplyAt  time.Time // when the last reply was sent; spaces the next reply
	EscalatedAt  *time.Time
	// NeedsReview flags a submission a human should look at (an unreadable or
	// low-confidence document); orthogonal to State, persists until resolved.
	NeedsReview bool
	// OnHold pauses replies, escalation, and auto-close while a human works the
	// case; set declaratively from the Hold folder, orthogonal to State.
	OnHold bool
	// DeliveryFailed marks a submission whose reply hard-bounced; suppresses
	// further replies until a human fixes the unreachable address.
	DeliveryFailed bool
	// NamedInsured is the account the submission is for (from the application);
	// the digest leads with it. Empty until extracted.
	NamedInsured string
	// EffectiveDate is the requested policy bind date; drives bind-window
	// escalation. Nil until known.
	EffectiveDate *time.Time
	// LastEscalatedAt is when the submission last escalated; unlike EscalatedAt it
	// survives a return to awaiting, so bind-window re-nudges can be rate-limited.
	LastEscalatedAt *time.Time
	Emails          []Email
	Documents       []Document
	MissingItems    []MissingItem
}

// NewSubmission creates a submission in the open state.
func NewSubmission(id, policyType, subject, fromAddress, fromName, threadKey string, now time.Time) Submission {
	return Submission{
		ID:           id,
		PolicyType:   policyType,
		State:        StateOpen,
		SubjectLine:  subject,
		FromAddress:  fromAddress,
		FromName:     fromName,
		ThreadKey:    threadKey,
		CreatedAt:    now,
		UpdatedAt:    now,
		LastActionAt: now,
	}
}

// TransitionTo moves the submission to next when the transition is allowed.
func (s *Submission) TransitionTo(next State, now time.Time) error {
	if s.State == next {
		s.UpdatedAt = now
		return nil
	}
	nexts, ok := allowed[s.State]
	if !ok {
		return fmt.Errorf("%w: unknown state %q", ErrInvalidTransition, s.State)
	}
	if _, ok := nexts[next]; !ok {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, s.State, next)
	}
	s.State = next
	s.UpdatedAt = now
	s.LastActionAt = now
	switch next {
	case StateEscalated:
		t := now
		s.EscalatedAt = &t
	case StateAwaiting, StateComplete:
		s.EscalatedAt = nil
	}
	return nil
}

// MarkAction resets the activity clock used for escalation.
func (s *Submission) MarkAction(now time.Time) {
	s.UpdatedAt = now
	s.LastActionAt = now
}

func (s *Submission) AttachEmail(e Email) {
	s.Emails = append(s.Emails, e)
}

func (s *Submission) AttachDocument(d Document) {
	s.Documents = append(s.Documents, d)
}
