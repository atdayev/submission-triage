package model

import (
	"errors"
	"fmt"
	"time"
)

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
	EscalatedAt  *time.Time
	Emails       []Email
	Documents    []Document
	MissingItems []MissingItem
}

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
