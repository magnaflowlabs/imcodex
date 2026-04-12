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

func fakeCodexEnv(t *testing.T, workdir string) (string, map[string]string) {
	t.Helper()

	root := t.TempDir()
	home := filepath.Join(root, "home")
	codexHome := filepath.Join(home, ".codex")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("MkdirAll(codexHome) error = %v", err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(binDir) error = %v", err)
	}

	argsFile := filepath.Join(root, "codex-args.txt")
	scriptPath := filepath.Join(binDir, "codex")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
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
