package email

import (
	"context"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/model"
)

func TestLogSender_ReturnsUniqueID(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	s := NewLogSender(log)
	id1, err := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "x@y"})
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "x@y"})
	if id1 == "" || id2 == "" {
		t.Error("ids should be non-empty")
	}
	if id1 == id2 {
		t.Errorf("ids should differ: %q vs %q", id1, id2)
	}
	if !strings.HasPrefix(id1, "log-sender-") {
		t.Errorf("expected log-sender prefix, got %q", id1)
	}
}
