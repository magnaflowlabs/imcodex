package codexcmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchCommandAttemptsResumeFirst(t *testing.T) {
	t.Parallel()

	workdir := filepath.Join(t.TempDir(), "repo 'resume first'")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	argsFile, env := fakeCodexEnv(t, workdir)
	cmd := exec.Command("sh", "-lc", LaunchCommand(workdir))
	cmd.Env = envSlice(env)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := readArgsLog(t, argsFile)
	if len(got) != 1 {
		t.Fatalf("invocations = %#v, want exactly one successful resume", got)
	}
	if !strings.Contains(got[0], "resume --last") {
		t.Fatalf("first invocation = %q, want resume --last", got[0])
	}
	if !strings.Contains(got[0], "-C "+workdir) {
		t.Fatalf("first invocation = %q, want cwd passed through", got[0])
	}
}

func TestLaunchCommandStartsFreshWhenResumeFails(t *testing.T) {
	t.Parallel()

	workdir := filepath.Join(t.TempDir(), "repo 'resume fails'")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	argsFile, env := fakeCodexEnv(t, workdir)
	env["IMCODEX_FAKE_RESUME_STATUS"] = "23"
	cmd := exec.Command("sh", "-lc", LaunchCommand(workdir))
	cmd.Env = envSlice(env)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := readArgsLog(t, argsFile)
	if len(got) != 2 {
		t.Fatalf("invocations = %#v, want resume then fresh", got)
	}
	if !strings.Contains(got[0], "resume --last") {
		t.Fatalf("first invocation = %q, want resume --last", got[0])
	}
	if strings.Contains(got[1], "resume --last") {
		t.Fatalf("second invocation = %q, want fresh launch without resume", got[1])
	}
	if !strings.Contains(got[1], "-a never -s danger-full-access --no-alt-screen -C "+workdir) {
		t.Fatalf("second invocation = %q, want fresh launch args", got[1])
	}
}

func TestLaunchCommandDoesNotFallbackAfterSuccessfulResume(t *testing.T) {
	t.Parallel()

	workdir := filepath.Join(t.TempDir(), "repo 'resume succeeds'")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	argsFile, env := fakeCodexEnv(t, workdir)
	env["IMCODEX_FAKE_RESUME_STATUS"] = "0"
	cmd := exec.Command("sh", "-lc", LaunchCommand(workdir))
	cmd.Env = envSlice(env)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := readArgsLog(t, argsFile)
	if len(got) != 1 {
		t.Fatalf("invocations = %#v, want successful resume only", got)
	}
	if !strings.Contains(got[0], "resume --last") {
		t.Fatalf("first invocation = %q, want resume path only", got[0])
	}
}

func TestFreshLaunchCommandSkipsResume(t *testing.T) {
	t.Parallel()

	workdir := filepath.Join(t.TempDir(), "repo 'fresh only'")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	argsFile, env := fakeCodexEnv(t, workdir)
	cmd := exec.Command("sh", "-lc", FreshLaunchCommand(workdir))
	cmd.Env = envSlice(env)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := readArgsLog(t, argsFile)
	if len(got) != 1 {
		t.Fatalf("invocations = %#v, want exactly one fresh launch", got)
	}
	if strings.Contains(got[0], "resume --last") {
		t.Fatalf("invocation = %q, want no resume", got[0])
	}
	if !strings.Contains(got[0], "-a never -s danger-full-access --no-alt-screen -C "+workdir) {
		t.Fatalf("invocation = %q, want fresh launch args", got[0])
	}
}

func TestLaunchCommandForSessionUsesManagedCodexHome(t *testing.T) {
	t.Parallel()

	workdir := filepath.Join(t.TempDir(), "repo 'managed home'")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	argsFile, env := fakeCodexEnv(t, workdir)
	cmd := exec.Command("sh", "-lc", LaunchCommandForSession(workdir, "Demo Session"))
	cmd.Env = envSlice(env)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := readArgsLog(t, argsFile)
	if len(got) != 1 {
		t.Fatalf("invocations = %#v, want one resume invocation", got)
	}
	managedHome := managedHomePath(env["CODEX_HOME"], workdir, "Demo Session")
	if !strings.Contains(got[0], "CODEX_HOME="+managedHome) {
		t.Fatalf("invocation = %q, want managed CODEX_HOME %q", got[0], managedHome)
	}
	if _, err := os.Lstat(filepath.Join(managedHome, "auth.json")); err != nil {
		t.Fatalf("managed auth.json missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(managedHome, "skills")); err != nil {
		t.Fatalf("managed skills missing: %v", err)
	}
}

func TestFreshLaunchCommandForSessionClearsManagedState(t *testing.T) {
	t.Parallel()

	workdir := filepath.Join(t.TempDir(), "repo 'fresh clear'")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	argsFile, env := fakeCodexEnv(t, workdir)
	managedHome := managedHomePath(env["CODEX_HOME"], workdir, "Demo Session")
	if err := os.MkdirAll(filepath.Join(managedHome, "sessions"), 0o755); err != nil {
		t.Fatalf("MkdirAll(sessions) error = %v", err)
	}
	for _, entry := range []string{
		filepath.Join(managedHome, "history.jsonl"),
		filepath.Join(managedHome, "state_5.sqlite"),
		filepath.Join(managedHome, "logs_2.sqlite"),
	} {
		if err := os.WriteFile(entry, []byte("stale"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", entry, err)
		}
	}

	cmd := exec.Command("sh", "-lc", FreshLaunchCommandForSession(workdir, "Demo Session"))
	cmd.Env = envSlice(env)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := readArgsLog(t, argsFile)
	if len(got) != 1 {
		t.Fatalf("invocations = %#v, want one fresh launch", got)
	}
	if strings.Contains(got[0], "resume --last") {
		t.Fatalf("invocation = %q, want no resume", got[0])
	}
	for _, stale := range []string{
		filepath.Join(managedHome, "history.jsonl"),
		filepath.Join(managedHome, "state_5.sqlite"),
		filepath.Join(managedHome, "logs_2.sqlite"),
		filepath.Join(managedHome, "sessions"),
	} {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("stale state %s still exists, stat err = %v", stale, err)
		}
	}
	for _, shared := range []string{
		filepath.Join(managedHome, "auth.json"),
		filepath.Join(managedHome, "config.toml"),
		filepath.Join(managedHome, "skills"),
		filepath.Join(managedHome, "AGENTS.md"),
	} {
		if _, err := os.Lstat(shared); err != nil {
			t.Fatalf("shared entry %s missing: %v", shared, err)
		}
	}
}

func TestLaunchCommandForSessionStripsProjectStateFromManagedConfig(t *testing.T) {
	t.Parallel()

	workdir := filepath.Join(t.TempDir(), "repo 'config scrub'")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	_, env := fakeCodexEnv(t, workdir)
	cmd := exec.Command("sh", "-lc", FreshLaunchCommandForSession(workdir, "Config Scrub"))
	cmd.Env = envSlice(env)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	managedConfig := filepath.Join(managedHomePath(env["CODEX_HOME"], workdir, "Config Scrub"), "config.toml")
	data, err := os.ReadFile(managedConfig)
	if err != nil {
		t.Fatalf("ReadFile(managed config) error = %v", err)
	}
	text := string(data)
	if strings.Contains(text, `[projects."/home/demo/project"]`) {
		t.Fatalf("managed config = %q, want project sections stripped", text)
	}
	for _, want := range []string{
		`model = "gpt-5.4"`,
		`[history]`,
		`persistence = "save-all"`,
		`[model_providers.codex]`,
		`[projects.` + tomlBasicString(workdir) + `]`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("managed config = %q, want substring %q", text, want)
		}
	}
}

func fakeCodexEnv(t *testing.T, workdir string) (string, map[string]string) {
	t.Helper()

	root := t.TempDir()
	home := filepath.Join(root, "home")
	codexHome := filepath.Join(home, ".codex")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("MkdirAll(codexHome) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(codexHome, "skills"), 0o755); err != nil {
		t.Fatalf("MkdirAll(skills) error = %v", err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(binDir) error = %v", err)
	}
	for name, content := range map[string]string{
		"auth.json": "auth",
		"config.toml": strings.Join([]string{
			`model = "gpt-5.4"`,
			``,
			`[history]`,
			`persistence = "save-all"`,
			``,
			`[model_providers.codex]`,
			`name = "codex"`,
			`base_url = "https://example.invalid"`,
			`wire_api = "responses"`,
			`requires_openai_auth = true`,
			``,
			`[projects."/home/demo/project"]`,
			`trust_level = "trusted"`,
			``,
		}, "\n"),
		"AGENTS.md": "global instructions\n",
	} {
		if err := os.WriteFile(filepath.Join(codexHome, name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}

	argsFile := filepath.Join(root, "codex-args.txt")
	scriptPath := filepath.Join(binDir, "codex")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\tCODEX_HOME=%%s\n' "$*" "${CODEX_HOME}" >> %s
if [ "$1" = "resume" ]; then
  exit "${IMCODEX_FAKE_RESUME_STATUS:-0}"
fi
exit 0
`, shellQuote(argsFile))
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(script) error = %v", err)
	}

	env := map[string]string{
		"HOME":       home,
		"CODEX_HOME": codexHome,
		"PATH":       binDir + ":" + os.Getenv("PATH"),
		"PWD":        workdir,
	}
	return argsFile, env
}

func readArgsLog(t *testing.T, argsFile string) []string {
	t.Helper()

	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	return strings.FieldsFunc(strings.TrimSpace(string(got)), func(r rune) bool { return r == '\n' })
}

func envSlice(values map[string]string) []string {
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}
