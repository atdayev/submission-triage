package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
)

func TestBuildPollerNilWhenIMAPUnconfigured(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	cfg := &config.Config{
		Outbound:   config.OutboundConfig{Provider: "log"},
		Database:   config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "triage.db")},
		Checklists: config.ChecklistsConfig{Directory: t.TempDir()},
	}
	built, err := Build(context.Background(), cfg, log, "../../migrations")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer built.DB.Close()
	defer built.Service.Shutdown()

	if built.Poller != nil {
		t.Error("Poller: got non-nil, want nil when IMAP unconfigured")
	}
}

func TestChooseSender(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	smtpCfg := config.SMTPConfig{Host: "smtp.example.com", FromAddress: "ops@x"}

	tests := []struct {
		name     string
		cfg      *config.Config
		wantName string // "" means expect an error
	}{
		{"explicit smtp", &config.Config{Outbound: config.OutboundConfig{Provider: "smtp"}, SMTP: smtpCfg}, "smtp"},
		{"provider is case-insensitive", &config.Config{Outbound: config.OutboundConfig{Provider: "SMTP"}, SMTP: smtpCfg}, "smtp"},
		{"explicit log sends nothing", &config.Config{Outbound: config.OutboundConfig{Provider: "log"}}, "log"},
		{"auto picks smtp when configured", &config.Config{SMTP: smtpCfg}, "smtp"},
		{"auto fails when no smtp configured", &config.Config{}, ""},
		{"explicit smtp without config errors", &config.Config{Outbound: config.OutboundConfig{Provider: "smtp"}}, ""},
		{"unknown provider errors", &config.Config{Outbound: config.OutboundConfig{Provider: "carrierpigeon"}}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := chooseSender(tt.cfg, log)
			if tt.wantName == "" {
				if err == nil {
					t.Fatalf("expected error, got sender %q", s.Name())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s.Name() != tt.wantName {
				t.Errorf("sender: got %q, want %q", s.Name(), tt.wantName)
			}
		})
	}
}
