package main

import (
	"strings"
	"testing"
)

func TestParseConfigReadsYAMLAndEnvFallback(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig(nil, envLookup(map[string]string{
		"LARK_APP_SECRET": "secret_env",
	}), readConfig(`
lark_app_id: cli_yaml
groups:
  - group_id: oc_1
    cwd: /srv/demo
`))
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}

	if cfg.larkAppID != "cli_yaml" {
		t.Fatalf("larkAppID = %q, want %q", cfg.larkAppID, "cli_yaml")
	}
	if cfg.larkAppSecret != "secret_env" {
		t.Fatalf("larkAppSecret = %q, want %q", cfg.larkAppSecret, "secret_env")
	}
	if cfg.larkBaseURL != "https://open.larksuite.com" {
		t.Fatalf("larkBaseURL = %q, want %q", cfg.larkBaseURL, "https://open.larksuite.com")
	}
	if len(cfg.groups) != 1 || cfg.groups[0].GroupID != "oc_1" || cfg.groups[0].CWD != "/srv/demo" {
		t.Fatalf("groups = %#v, want one normalized group", cfg.groups)
	}
}

func TestParseConfigReadsBaseURL(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig(nil, nil, readConfig(`
lark_app_id: cli_yaml
lark_app_secret: secret_yaml
lark_base_url: https://open.larksuite.com
groups:
  - group_id: oc_1
    cwd: /srv/demo
`))
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if cfg.larkBaseURL != "https://open.larksuite.com" {
		t.Fatalf("larkBaseURL = %q, want %q", cfg.larkBaseURL, "https://open.larksuite.com")
	}
}

func TestParseConfigFlagOverridesDefaultPath(t *testing.T) {
	t.Parallel()

	var gotPath string
	_, err := parseConfig([]string{"-config", "/tmp/imcodex.yaml"}, envLookup(map[string]string{
		"LARK_APP_ID":     "cli_env",
		"LARK_APP_SECRET": "secret_env",
	}), func(path string) ([]byte, error) {
		gotPath = path
		return []byte("groups:\n  - group_id: oc_1\n    cwd: /srv/demo\n"), nil
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if gotPath != "/tmp/imcodex.yaml" {
		t.Fatalf("config path = %q, want %q", gotPath, "/tmp/imcodex.yaml")
	}
}

func TestParseConfigRejectsDuplicateGroup(t *testing.T) {
	t.Parallel()

	_, err := parseConfig(nil, envLookup(map[string]string{
		"LARK_APP_ID":     "cli_env",
		"LARK_APP_SECRET": "secret_env",
	}), readConfig(`
groups:
  - group_id: oc_1
    cwd: /srv/a
  - group_id: oc_1
    cwd: /srv/b
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate group_id") {
		t.Fatalf("parseConfig() error = %v, want duplicate group_id", err)
	}
}

func TestParseConfigRejectsUnknownField(t *testing.T) {
	t.Parallel()

	_, err := parseConfig(nil, envLookup(map[string]string{
		"LARK_APP_ID":     "cli_env",
		"LARK_APP_SECRET": "secret_env",
	}), readConfig("unknown: true\n"))
	if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("parseConfig() error = %v, want unknown-field error", err)
	}
}

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func readConfig(content string) func(string) ([]byte, error) {
	return func(string) ([]byte, error) {
		return []byte(content), nil
	}
}
