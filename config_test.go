package main

import (
	"os"
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
	if !cfg.interruptOnNewMessage {
		t.Fatal("interruptOnNewMessage = false, want default true")
	}
	if cfg.platform != "lark" {
		t.Fatalf("platform = %q, want lark", cfg.platform)
	}
	if len(cfg.groups) != 1 || cfg.groups[0].GroupID != "oc_1" || cfg.groups[0].CWD != "/srv/demo" {
		t.Fatalf("groups = %#v, want one normalized group", cfg.groups)
	}
}

func TestParseConfigReadsJobsAndResolvesPromptFile(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig([]string{"-config", "/srv/imcodex/config.yaml"}, envLookup(map[string]string{
		"LARK_APP_ID":     "cli_env",
		"LARK_APP_SECRET": "secret_env",
	}), readConfig(`
groups:
  - group_id: oc_1
    cwd: /srv/demo
    jobs:
      - name: hourly_review
        schedule: "1 * * * *"
        prompt_file: ./prompts/hourly_review.md
`))
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}

	if len(cfg.groups) != 1 || len(cfg.groups[0].Jobs) != 1 {
		t.Fatalf("groups = %#v, want one group with one job", cfg.groups)
	}
	if got, want := cfg.groups[0].Jobs[0].PromptFile, "/srv/imcodex/prompts/hourly_review.md"; got != want {
		t.Fatalf("prompt_file = %q, want %q", got, want)
	}
}

func TestParseConfigReadsCommandJobAndResolvesPathsFromCWD(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig([]string{"-config", "/srv/imcodex/config.yaml"}, envLookup(map[string]string{
		"LARK_APP_ID":     "cli_env",
		"LARK_APP_SECRET": "secret_env",
	}), readConfig(`
groups:
  - group_id: oc_1
    cwd: /srv/demo
    jobs:
      - name: hl_stack_cycle
        schedule: "1 * * * *"
        command: ./ops/run_dry_cycle.sh
        artifacts_dir: ./.imcodex/jobs/hl_stack_cycle
        summary_file: ./.imcodex/jobs/hl_stack_cycle/latest-summary.md
`))
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}

	job := cfg.groups[0].Jobs[0]
	if got, want := job.Command, "./ops/run_dry_cycle.sh"; got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
	if got, want := job.ArtifactsDir, "/srv/demo/.imcodex/jobs/hl_stack_cycle"; got != want {
		t.Fatalf("artifacts_dir = %q, want %q", got, want)
	}
	if got, want := job.SummaryFile, "/srv/demo/.imcodex/jobs/hl_stack_cycle/latest-summary.md"; got != want {
		t.Fatalf("summary_file = %q, want %q", got, want)
	}
}

func TestParseConfigReadsGlobalSessionCommandAndSessionNames(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig([]string{"-config", "/srv/imcodex/config.yaml"}, envLookup(map[string]string{
		"LARK_APP_ID":     "cli_env",
		"LARK_APP_SECRET": "secret_env",
	}), readConfig(`
session_command: /usr/local/bin/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent codex
groups:
  - group_id: oc_1
    cwd: /srv/demo
    session_name: main-demo
    jobs:
      - name: hourly_review
        schedule: "1 * * * *"
        prompt_file: ./prompts/hourly_review.md
        session_name: job-demo
`))
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}

	if got, want := cfg.sessionCommand, "/usr/local/bin/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent codex"; got != want {
		t.Fatalf("config session_command = %q, want %q", got, want)
	}
	group := cfg.groups[0]
	if got, want := group.SessionName, "main-demo"; got != want {
		t.Fatalf("group session_name = %q, want %q", got, want)
	}
	job := group.Jobs[0]
	if got, want := job.SessionName, "job-demo"; got != want {
		t.Fatalf("job session_name = %q, want %q", got, want)
	}
}

func TestParseConfigReadsInterruptOnNewMessage(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig(nil, envLookup(map[string]string{
		"LARK_APP_ID":     "cli_env",
		"LARK_APP_SECRET": "secret_env",
	}), readConfig(`
interrupt_on_new_message: false
groups:
  - group_id: oc_1
    cwd: /srv/demo
`))
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if cfg.interruptOnNewMessage {
		t.Fatal("interruptOnNewMessage = true, want false")
	}
}

func TestParseConfigReadsTelegramMode(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig(nil, nil, readConfig(`
platform: telegram
telegram_bot_token: 123456:abc
groups:
  - group_id: -100123
    cwd: /srv/demo
`))
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if cfg.platform != "telegram" {
		t.Fatalf("platform = %q, want telegram", cfg.platform)
	}
	if cfg.telegramBotToken != "123456:abc" {
		t.Fatalf("telegramBotToken = %q, want token", cfg.telegramBotToken)
	}
	if cfg.telegramBaseURL != "https://api.telegram.org" {
		t.Fatalf("telegramBaseURL = %q, want default Telegram API URL", cfg.telegramBaseURL)
	}
}

func TestParseConfigRejectsTelegramWithoutToken(t *testing.T) {
	t.Parallel()

	_, err := parseConfig(nil, nil, readConfig(`
platform: telegram
groups:
  - group_id: -100123
    cwd: /srv/demo
`))
	if err == nil || !strings.Contains(err.Error(), "telegram_bot_token") {
		t.Fatalf("parseConfig() error = %v, want telegram token error", err)
	}
}

func TestParseConfigRejectsUnsupportedPlatform(t *testing.T) {
	t.Parallel()

	_, err := parseConfig(nil, nil, readConfig(`
platform: discord
lark_app_id: cli_yaml
lark_app_secret: secret_yaml
groups:
  - group_id: oc_1
    cwd: /srv/demo
`))
	if err == nil || !strings.Contains(err.Error(), "unsupported platform") {
		t.Fatalf("parseConfig() error = %v, want unsupported platform error", err)
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

func TestParseConfigFallsBackToUserHomeConfig(t *testing.T) {
	t.Parallel()

	var gotPaths []string
	cfg, err := parseConfig(nil, envLookup(map[string]string{
		"HOME":            "/home/demo",
		"LARK_APP_ID":     "cli_env",
		"LARK_APP_SECRET": "secret_env",
	}), func(path string) ([]byte, error) {
		gotPaths = append(gotPaths, path)
		switch path {
		case defaultConfigPath:
			return nil, os.ErrNotExist
		case "/home/demo/.imcodex.yaml":
			return []byte("groups:\n  - group_id: oc_1\n    cwd: /srv/demo\n"), nil
		default:
			t.Fatalf("unexpected config path read: %s", path)
			return nil, nil
		}
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}

	if cfg.path != "/home/demo/.imcodex.yaml" {
		t.Fatalf("cfg.path = %q, want %q", cfg.path, "/home/demo/.imcodex.yaml")
	}
	if got, want := strings.Join(gotPaths, ","), "imcodex.yaml,/home/demo/.imcodex.yaml"; got != want {
		t.Fatalf("config paths = %q, want %q", got, want)
	}
}

func TestParseConfigReturnsHelpfulErrorWhenDefaultConfigsAreMissing(t *testing.T) {
	t.Parallel()

	_, err := parseConfig(nil, envLookup(map[string]string{
		"HOME": "/home/demo",
	}), func(string) ([]byte, error) {
		return nil, os.ErrNotExist
	})
	if err == nil || !strings.Contains(err.Error(), "config not found; tried imcodex.yaml, /home/demo/.imcodex.yaml") {
		t.Fatalf("parseConfig() error = %v, want missing-config paths", err)
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

func TestParseConfigRejectsDuplicateJobNameInGroup(t *testing.T) {
	t.Parallel()

	_, err := parseConfig(nil, envLookup(map[string]string{
		"LARK_APP_ID":     "cli_env",
		"LARK_APP_SECRET": "secret_env",
	}), readConfig(`
groups:
  - group_id: oc_1
    cwd: /srv/a
    jobs:
      - name: hourly_review
        schedule: "1 * * * *"
        prompt_file: /srv/a/prompts/hourly.md
      - name: hourly_review
        schedule: "5 * * * *"
        prompt_file: /srv/a/prompts/hourly2.md
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate job name") {
		t.Fatalf("parseConfig() error = %v, want duplicate job name error", err)
	}
}

func TestParseConfigRejectsJobWithoutPromptFileOrCommand(t *testing.T) {
	t.Parallel()

	_, err := parseConfig(nil, envLookup(map[string]string{
		"LARK_APP_ID":     "cli_env",
		"LARK_APP_SECRET": "secret_env",
	}), readConfig(`
groups:
  - group_id: oc_1
    cwd: /srv/a
    jobs:
      - name: hourly_review
        schedule: "1 * * * *"
`))
	if err == nil || !strings.Contains(err.Error(), "must set one of prompt_file or command") {
		t.Fatalf("parseConfig() error = %v, want prompt_file-or-command error", err)
	}
}

func TestParseConfigRejectsJobWithPromptFileAndCommand(t *testing.T) {
	t.Parallel()

	_, err := parseConfig(nil, envLookup(map[string]string{
		"LARK_APP_ID":     "cli_env",
		"LARK_APP_SECRET": "secret_env",
	}), readConfig(`
groups:
  - group_id: oc_1
    cwd: /srv/a
    jobs:
      - name: hourly_review
        schedule: "1 * * * *"
        prompt_file: /srv/a/prompts/hourly.md
        command: ./ops/run_dry_cycle.sh
`))
	if err == nil || !strings.Contains(err.Error(), "only one of prompt_file or command") {
		t.Fatalf("parseConfig() error = %v, want prompt_file-vs-command error", err)
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
