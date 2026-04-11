package tmuxctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientSendTextTracksControlPaneAfterPaneReorder(t *testing.T) {
	t.Parallel()

	requireTmux(t)

	ctx := context.Background()
	client := newTestClient()
	spec := SessionSpec{
		SessionName: fmt.Sprintf("imcodex-test-%d", time.Now().UnixNano()),
		CWD:         t.TempDir(),
		StartupWait: 50 * time.Millisecond,
	}

	created, err := client.EnsureSession(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureSession() error = %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	t.Cleanup(func() {
		_ = client.run(ctx, "kill-session", "-t", spec.SessionName)
	})

	controlPane, err := client.controlPaneTarget(ctx, spec.SessionName)
	if err != nil {
		t.Fatalf("controlPaneTarget() error = %v", err)
	}

	if err := client.run(ctx, "split-window", "-d", "-t", controlPane, "sleep 30"); err != nil {
		t.Fatalf("split-window error = %v", err)
	}
	if err := client.run(ctx, "swap-pane", "-s", controlPane, "-t", spec.SessionName+":0.1"); err != nil {
		t.Fatalf("swap-pane error = %v", err)
	}

	zeroPane, err := client.output(ctx, "display-message", "-p", "-t", spec.SessionName+":0.0", "#{pane_id}")
	if err != nil {
		t.Fatalf("display-message() error = %v", err)
	}
	if strings.TrimSpace(zeroPane) == controlPane {
		t.Fatal("control pane is still session:0.0, want pane reorder to move it away from fixed index")
	}

	if err := client.SendText(ctx, spec.SessionName, "after swap"); err != nil {
		t.Fatalf("SendText() error = %v", err)
	}
	waitForCaptureContains(t, client, spec.SessionName, "after swap")
}

func TestClientEnsureSessionRecreatesMissingControlPane(t *testing.T) {
	t.Parallel()

	requireTmux(t)

	ctx := context.Background()
	client := newTestClient()
	spec := SessionSpec{
		SessionName: fmt.Sprintf("imcodex-test-%d", time.Now().UnixNano()),
		CWD:         t.TempDir(),
		StartupWait: 50 * time.Millisecond,
	}

	if _, err := client.EnsureSession(ctx, spec); err != nil {
		t.Fatalf("EnsureSession() error = %v", err)
	}
	t.Cleanup(func() {
		_ = client.run(ctx, "kill-session", "-t", spec.SessionName)
	})

	if err := client.run(ctx, "new-window", "-d", "-t", spec.SessionName, "sleep 30"); err != nil {
		t.Fatalf("new-window error = %v", err)
	}

	controlPane, err := client.controlPaneTarget(ctx, spec.SessionName)
	if err != nil {
		t.Fatalf("controlPaneTarget() error = %v", err)
	}
	if err := client.run(ctx, "kill-pane", "-t", controlPane); err != nil {
		t.Fatalf("kill-pane error = %v", err)
	}

	recreated, err := client.EnsureSession(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureSession(recreate) error = %v", err)
	}
	if recreated {
		t.Fatal("recreated = true, want false because the tmux session already existed")
	}

	newControlPane, err := client.controlPaneTarget(ctx, spec.SessionName)
	if err != nil {
		t.Fatalf("controlPaneTarget(recreated) error = %v", err)
	}
	if newControlPane == "" || newControlPane == controlPane {
		t.Fatalf("newControlPane = %q, want a different recreated pane", newControlPane)
	}

	if err := client.SendText(ctx, spec.SessionName, "after recreate"); err != nil {
		t.Fatalf("SendText() error = %v", err)
	}
	waitForCaptureContains(t, client, spec.SessionName, "after recreate")
}

func TestDefaultLaunchCommandUsesNeverApprovalAndDangerFullAccess(t *testing.T) {
	t.Parallel()

	got := defaultLaunchCommand(SessionSpec{CWD: "/srv/demo"})
	want := "exec 'codex' '-a' 'never' '-s' 'danger-full-access' '--no-alt-screen' '-C' '/srv/demo'"
	if got != want {
		t.Fatalf("defaultLaunchCommand() = %q, want %q", got, want)
	}
}

func TestExpandLaunchCommandTemplate(t *testing.T) {
	t.Parallel()

	got := expandLaunchCommandTemplate("exec '/srv/imcodex/imcodex' 'internal-run-docker-codex' '--workspace' '{cwd}' '--session' '{session_name}' '--group' '{group_id}' '--job' '{job_name}'", SessionSpec{
		SessionName: "demo-session",
		CWD:         "/srv/demo",
		GroupID:     "oc_1",
		JobName:     "hourly_review",
	})
	want := "exec '/srv/imcodex/imcodex' 'internal-run-docker-codex' '--workspace' '/srv/demo' '--session' 'demo-session' '--group' 'oc_1' '--job' 'hourly_review'"
	if got != want {
		t.Fatalf("expandLaunchCommandTemplate() = %q, want %q", got, want)
	}
}

func TestClientSendTextUsesBracketedPaste(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	contentPath := filepath.Join(dir, "buffer.txt")
	scriptPath := filepath.Join(dir, "tmux")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
case "$1" in
  load-buffer)
    cat "$4" > %s
    ;;
  show-options)
    printf '%%%%42\n'
    ;;
  display-message)
    printf '%%%%42\n'
    ;;
esac
`, shellQuote(logPath), shellQuote(contentPath))
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	client := New()
	client.bin = scriptPath
	client.enterWait = 0

	if err := client.SendText(context.Background(), "demo", "line1\nline2"); err != nil {
		t.Fatalf("SendText() error = %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "load-buffer -b imcodex-demo") {
		t.Fatalf("tmux log = %q, want load-buffer from temp file", logText)
	}
	if strings.Contains(logText, "set-buffer -b imcodex-demo") {
		t.Fatalf("tmux log = %q, want set-buffer removed for long-safe input", logText)
	}
	if !strings.Contains(logText, "paste-buffer -p -d -b imcodex-demo -t %42") {
		t.Fatalf("tmux log = %q, want bracketed paste", logText)
	}

	contentData, err := os.ReadFile(contentPath)
	if err != nil {
		t.Fatalf("ReadFile(buffer) error = %v", err)
	}
	if got, want := string(contentData), "line1\nline2"; got != want {
		t.Fatalf("buffer content = %q, want %q", got, want)
	}
}

func TestClientInterruptUsesEscapeAndCtrlC(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	scriptPath := filepath.Join(dir, "tmux")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
case "$1" in
  show-options)
    printf '%%%%42\n'
    ;;
  display-message)
    printf '%%%%42\n'
    ;;
esac
`, shellQuote(logPath))
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	client := New()
	client.bin = scriptPath

	if err := client.Interrupt(context.Background(), "demo"); err != nil {
		t.Fatalf("Interrupt() error = %v", err)
	}
	if err := client.ForceInterrupt(context.Background(), "demo"); err != nil {
		t.Fatalf("ForceInterrupt() error = %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "send-keys -t %42 Escape") {
		t.Fatalf("tmux log = %q, want Escape interrupt", logText)
	}
	if !strings.Contains(logText, "send-keys -t %42 C-c") {
		t.Fatalf("tmux log = %q, want C-c force interrupt", logText)
	}
}

func TestEnsureSessionRejectsMissingWorkingDirectory(t *testing.T) {
	t.Parallel()

	client := New()
	_, err := client.EnsureSession(context.Background(), SessionSpec{
		SessionName: "imcodex-test-missing-cwd",
		CWD:         "/definitely/missing/imcodex",
		StartupWait: 0,
	})
	if err == nil || !strings.Contains(err.Error(), "working directory does not exist") {
		t.Fatalf("EnsureSession() error = %v, want missing working directory", err)
	}
}

func TestSetControlPaneMarksPaneRole(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	scriptPath := filepath.Join(dir, "tmux")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
case "$1" in
  set-option)
    exit 0
    ;;
esac
`, shellQuote(logPath))
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	client := New()
	client.bin = scriptPath
	if err := client.setControlPane(context.Background(), "demo", "%42"); err != nil {
		t.Fatalf("setControlPane() error = %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "set-option -q -t demo @imcodex-control-pane %42") {
		t.Fatalf("tmux log = %q, want session control pane option", logText)
	}
	if !strings.Contains(logText, "set-option -p -t %42 @imcodex-pane-role control") {
		t.Fatalf("tmux log = %q, want pane role marker", logText)
	}
}

func TestFindExistingControlPanePrefersPaneRoleMarker(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
case "$1" in
  list-panes)
    printf '%%10\tcontrol\tdocker\n%%11\t\tbash\n'
    ;;
esac
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	client := New()
	client.bin = scriptPath
	got, err := client.findExistingControlPane(context.Background(), "demo", false)
	if err != nil {
		t.Fatalf("findExistingControlPane() error = %v", err)
	}
	if got != "%10" {
		t.Fatalf("findExistingControlPane() = %q, want %%10", got)
	}
}

func TestValidateWorkingDirectoryRejectsFile(t *testing.T) {
	t.Parallel()

	file, err := os.CreateTemp(t.TempDir(), "imcodex-file")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	file.Close()

	err = validateWorkingDirectory(file.Name())
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("validateWorkingDirectory() error = %v, want not-a-directory", err)
	}
}

func newTestClient() *Client {
	client := New()
	client.enterWait = 0
	client.launchCommand = func(SessionSpec) string {
		return "sh -lc 'printf \"›\\n\"; exec cat'"
	}
	return client
}

func TestClientEnsureSessionWaitsForPromptForHostRuntime(t *testing.T) {
	t.Parallel()

	requireTmux(t)

	ctx := context.Background()
	client := New()
	client.enterWait = 0
	client.launchCommand = func(SessionSpec) string {
		return "sh -lc 'sleep 0.25; printf \"›\\n\"; exec cat'"
	}
	spec := SessionSpec{
		SessionName: fmt.Sprintf("imcodex-test-%d", time.Now().UnixNano()),
		CWD:         t.TempDir(),
		StartupWait: 10 * time.Millisecond,
	}

	startedAt := time.Now()
	created, err := client.EnsureSession(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureSession() error = %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	t.Cleanup(func() {
		_ = client.run(ctx, "kill-session", "-t", spec.SessionName)
	})

	if elapsed := time.Since(startedAt); elapsed < 200*time.Millisecond {
		t.Fatalf("EnsureSession() elapsed = %s, want wait for prompt before returning", elapsed)
	}
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

func waitForCaptureContains(t *testing.T, client *Client, session string, want string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := client.Capture(context.Background(), session, 200)
		if err == nil && strings.Contains(got, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, err := client.Capture(context.Background(), session, 200)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	t.Fatalf("Capture() = %q, want substring %q", got, want)
}
