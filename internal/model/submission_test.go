package model

import (
	"errors"
	"testing"
	"time"
)

func TestSubmission_TransitionTo(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		from    State
		to      State
		wantErr error
	}{
		{"open to awaiting", StateOpen, StateAwaiting, nil},
		{"open to complete", StateOpen, StateComplete, nil},
		{"open to escalated", StateOpen, StateEscalated, nil},
		{"open to closed", StateOpen, StateClosed, nil},
		{"awaiting to complete", StateAwaiting, StateComplete, nil},
		{"awaiting to escalated", StateAwaiting, StateEscalated, nil},
		{"awaiting back to awaiting", StateAwaiting, StateAwaiting, nil},
		{"awaiting to closed", StateAwaiting, StateClosed, nil},
		{"complete to awaiting", StateComplete, StateAwaiting, nil},
		{"complete to closed", StateComplete, StateClosed, nil},
		{"complete to escalated rejected", StateComplete, StateEscalated, ErrInvalidTransition},
		{"escalated to awaiting", StateEscalated, StateAwaiting, nil},
		{"escalated to complete", StateEscalated, StateComplete, nil},
		{"escalated to closed", StateEscalated, StateClosed, nil},
		{"closed to awaiting rejected", StateClosed, StateAwaiting, ErrInvalidTransition},
		{"closed to complete rejected", StateClosed, StateComplete, ErrInvalidTransition},
		{"closed to escalated rejected", StateClosed, StateEscalated, ErrInvalidTransition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Submission{ID: "x", State: tc.from}
			err := s.TransitionTo(tc.to, now)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if s.State != tc.to {
				t.Fatalf("state: got %s, want %s", s.State, tc.to)
			}
		})
	}
}

func TestSubmission_TransitionTo_Escalated_SetsEscalatedAt(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := Submission{ID: "x", State: StateAwaiting}
	if err := s.TransitionTo(StateEscalated, now); err != nil {
		t.Fatal(err)
	}
	if s.EscalatedAt == nil || !s.EscalatedAt.Equal(now) {
		t.Fatalf("EscalatedAt: got %v, want %v", s.EscalatedAt, now)
	}
}

func TestSubmission_TransitionTo_OutOfEscalated_ClearsEscalatedAt(t *testing.T) {
	escalatedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	later := escalatedAt.Add(time.Hour)
	cases := []struct {
		name string
		to   State
	}{
		{"escalated to awaiting clears", StateAwaiting},
		{"escalated to complete clears", StateComplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Submission{ID: "x", State: StateEscalated, EscalatedAt: &escalatedAt}
			if err := s.TransitionTo(tc.to, later); err != nil {
				t.Fatal(err)
			}
			if s.EscalatedAt != nil {
				t.Fatalf("EscalatedAt should be cleared, got %v", s.EscalatedAt)
			}
		})
	}
}

func TestSubmission_TransitionTo_SameStateUpdatesTimestampOnly(t *testing.T) {
	earlier := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)
	s := Submission{ID: "x", State: StateAwaiting, UpdatedAt: earlier, LastActionAt: earlier}

	if err := s.TransitionTo(StateAwaiting, later); err != nil {
		t.Fatal(err)
	}
	if !s.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt: got %v, want %v", s.UpdatedAt, later)
	}
	if !s.LastActionAt.Equal(earlier) {
		t.Errorf("LastActionAt should not move on same-state: got %v", s.LastActionAt)
	}
}

func TestSubmission_AttachAndAction(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := NewSubmission("id1", "cgl", "subject", "x@y", "X", "tk", now)
	if s.State != StateOpen {
		t.Fatalf("new state: got %s", s.State)
	}
	s.AttachEmail(Email{DeterministicID: "d1"})
	if len(s.Emails) != 1 {
		t.Fatalf("attach email: got %d", len(s.Emails))
	}
	s.AttachDocument(Document{ID: "x"})
	if len(s.Documents) != 1 {
		t.Fatal("attach document failed")
	}
	later := now.Add(time.Hour)
	s.MarkAction(later)
	if !s.LastActionAt.Equal(later) {
		t.Fatal("mark action did not update last_action_at")
	}
}
