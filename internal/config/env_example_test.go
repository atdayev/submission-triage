package config

import (
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// envExamplePath is the shipped sample config — the only place most operators
// ever look for the full list of settings.
var envExamplePath = filepath.Join("..", "..", ".env.example")

// deprecatedVars are read but warned about at startup. They are documented in
// .env.example as comments rather than live lines, so nobody copies them forward.
var deprecatedVars = map[string]bool{
	"IMAP_COMPLETE_LABEL":              true,
	"ESCALATION_DIGEST_INTERVAL_HOURS": true,
	"ESCALATION_DIGEST_RECIPIENT":      true,
}

// declaredEnvVars walks Config's nested structs and collects every `env` tag.
func declaredEnvVars(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type)
				continue
			}
			if name, ok := f.Tag.Lookup("env"); ok {
				out[name] = f.Tag.Get("envDefault")
			}
		}
	}
	walk(reflect.TypeFor[Config]())
	if len(out) == 0 {
		t.Fatal("no env tags found; the walker is broken, not the sample file")
	}
	return out
}

// A setting absent from .env.example is a setting operators never discover. This
// caught 28 undocumented variables, including the mailbox folder names the README
// advertises and the entire OAuth authentication mode.
func TestEnvExample_DocumentsEverySetting(t *testing.T) {
	declared := declaredEnvVars(t)
	listed, err := godotenv.Read(envExamplePath)
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	var missing []string
	for name := range declared {
		if deprecatedVars[name] {
			if _, live := listed[name]; live {
				t.Errorf("%s is deprecated but set as a live line in .env.example; document it as a comment", name)
			}
			continue
		}
		if _, ok := listed[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d setting(s) missing from .env.example:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}

	var phantom []string
	for name := range listed {
		if _, ok := declared[name]; !ok {
			phantom = append(phantom, name)
		}
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Errorf(".env.example sets %d variable(s) the service never reads:\n  %s",
			len(phantom), strings.Join(phantom, "\n  "))
	}
}

// A sample value that disagrees with the code's default silently teaches the wrong
// baseline: an operator copies the file and gets behaviour the docs don't describe.
func TestEnvExample_ValuesMatchCodeDefaults(t *testing.T) {
	declared := declaredEnvVars(t)
	listed, err := godotenv.Read(envExamplePath)
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	for name, sample := range listed {
		def, ok := declared[name]
		if !ok || def == "" || sample == "" {
			continue // no default, or deliberately blank (secrets, opt-ins)
		}
		if sample != def {
			t.Errorf("%s: .env.example says %q, code default is %q", name, sample, def)
		}
	}
}

// The shipped sample must produce a config the service accepts. Otherwise the very
// first thing a new operator does — copy it to .env — fails at startup.
func TestEnvExample_ParsesAndValidates(t *testing.T) {
	listed, err := godotenv.Read(envExamplePath)
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	for k, v := range listed {
		t.Setenv(k, v)
	}

	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("the shipped .env.example does not parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the shipped .env.example does not validate: %v", err)
	}

	// spot-check that values actually landed, so a silently-empty parse can't pass
	if cfg.IMAP.Mailbox != "INBOX" {
		t.Errorf("IMAP_MAILBOX did not load: got %q", cfg.IMAP.Mailbox)
	}
	if cfg.IMAP.FolderHold != "Hold" {
		t.Errorf("IMAP_FOLDER_HOLD did not load: got %q", cfg.IMAP.FolderHold)
	}
	if cfg.Reply.RepliesEnabled == nil || !*cfg.Reply.RepliesEnabled {
		t.Error("REPLIES_ENABLED should load as true; the sample must not ship replies disabled")
	}
}
