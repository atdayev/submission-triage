//go:build e2e
// +build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/app"
	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	"github.com/atdayev/submission-triage/pkg/postmarkeml"
	"github.com/atdayev/submission-triage/pkg/telemetry"
)

const e2eWebhookSecret = "e2e-secret"

type ingestResp struct {
	SubmissionID string `json:"submission_id"`
	State        string `json:"state"`
	Duplicate    bool   `json:"duplicate"`
}

var expectations = map[string]model.State{
	"01_acme_cgl_complete.eml":            model.StateComplete,
	"02_riverbend_bop_complete.eml":       model.StateComplete,
	"03_pinegate_wc_complete.eml":         model.StateComplete,
	"04_oakview_cgl_missing_lossrun.eml":  model.StateAwaiting,
	"05_bluefin_property_missing_two.eml": model.StateAwaiting,
	"06_nimbus_cyber_missing_three.eml":   model.StateAwaiting,
	"07_redhill_wc_missing_one.eml":       model.StateAwaiting,
	"08_oakview_followup_lossrun.eml":     model.StateComplete,
	"09_bluefin_followup_remainder.eml":   model.StateComplete,
	"10_redhill_followup_payroll.eml":     model.StateComplete,
	"11_stalecase_zenith_cgl.eml":         model.StateAwaiting,
	"12_stalecase_brookline_bop.eml":      model.StateAwaiting,
}

func TestReplayCorpus(t *testing.T) {
	repoRoot := repoRootDir(t)
	ctx := context.Background()
	log := logrus.NewEntry(logrus.New())

	cfg := &config.Config{
		Service:    config.ServiceConfig{Name: "e2e"},
		HTTP:       config.HTTPConfig{Port: 0},
		Database:   config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "e2e.db")},
		Postmark:   config.PostmarkConfig{FromAddress: "test@triage.example", FromName: "Test", WebhookSecret: e2eWebhookSecret},
		Checklists: config.ChecklistsConfig{Directory: filepath.Join(repoRoot, "checklists")},
		Escalation: config.EscalationConfig{IntervalMinutes: 60, ThresholdHours: 72},
		Retry:      config.RetryConfig{Attempts: 1, BaseDelayMs: 1},
	}

	built, err := app.Build(ctx, cfg, log, filepath.Join(repoRoot, "migrations"), telemetry.NoopMetrics())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer built.DB.Close()

	server := httptest.NewServer(built.Router)
	defer server.Close()

	results := postCorpus(t, server.URL, filepath.Join(repoRoot, "testdata", "eml"))

	t.Run("ingest_results_match_expectations", func(t *testing.T) {
		for name, want := range expectations {
			name, want := name, want
			t.Run(name, func(t *testing.T) {
				got, ok := results[name]
				if !ok {
					t.Fatal("no result recorded")
				}
				if got.State != string(want) {
					t.Errorf("state: got %s, want %s", got.State, want)
				}
			})
		}
	})

	t.Run("audit_log_contains_expected_events", func(t *testing.T) {
		repo := repository.NewRepository(built.DB, log)
		for name, r := range results {
			name, r := name, r
			t.Run(name, func(t *testing.T) {
				entries, err := repo.Audit.ListBySubmission(ctx, r.SubmissionID)
				if err != nil {
					t.Fatalf("audit list: %v", err)
				}
				seen := map[model.EventType]int{}
				for _, e := range entries {
					seen[e.EventType]++
				}
				if seen[model.EventEmailReceived] < 1 {
					t.Error("missing EventEmailReceived")
				}
				if seen[model.EventChecklistEvaluated] < 1 {
					t.Error("missing EventChecklistEvaluated")
				}
			})
		}
	})

	// stale-case fixtures are dated weeks back, so they escalate on a worker run
	t.Run("stale_submissions_escalate", func(t *testing.T) {
		if err := built.Service.CheckEscalations(ctx); err != nil {
			t.Fatalf("check escalations: %v", err)
		}
		subs := repository.NewSubmissionRepository(built.DB, log)
		for _, name := range []string{
			"11_stalecase_zenith_cgl.eml",
			"12_stalecase_brookline_bop.eml",
		} {
			r, ok := results[name]
			if !ok {
				t.Fatalf("%s: no result recorded", name)
			}
			got, err := subs.GetByID(ctx, r.SubmissionID)
			if err != nil {
				t.Fatalf("%s: get: %v", name, err)
			}
			if got.State != model.StateEscalated {
				t.Errorf("%s: state = %s, want escalated", name, got.State)
			}
			if got.EscalatedAt == nil {
				t.Errorf("%s: EscalatedAt not set", name)
			}
		}
	})
}

func postCorpus(t *testing.T, baseURL, emlDir string) map[string]ingestResp {
	t.Helper()
	names := listEMLs(t, emlDir)
	client := &http.Client{Timeout: 10 * time.Second}
	results := make(map[string]ingestResp, len(names))

	for _, name := range names {
		payload, err := postmarkeml.FromFile(filepath.Join(emlDir, name))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		raw, _ := json.Marshal(payload)

		req, err := http.NewRequest(http.MethodPost, baseURL+"/webhooks/postmark", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: new request: %v", name, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Webhook-Secret", e2eWebhookSecret)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: post: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d body=%s", name, resp.StatusCode, string(body))
		}
		var r ingestResp
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatalf("%s: decode: %v body=%s", name, err, string(body))
		}
		results[name] = r
	}
	return results
}

func listEMLs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".eml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func repoRootDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}
