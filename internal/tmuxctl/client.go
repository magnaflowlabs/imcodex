package tmuxctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	controlPaneOption = "@imcodex-control-pane"
	controlPaneRole   = "@imcodex-pane-role"
	controlWindowName = "imcodex"
	readyPollEvery    = 250 * time.Millisecond
	hostReadyWait     = 30 * time.Second
	dockerReadyWait   = 2 * time.Minute
)

type SessionSpec struct {
	SessionName                 string
	CWD                         string
	GroupID                     string
	JobName                     string
	LaunchCommand               string
	StartupWait                 time.Duration
	AutoPressEnterOnTrustPrompt bool
}

type Client struct {
	bin           string
	enterWait     time.Duration
	launchCommand func(SessionSpec) string
}

func New() *Client {
	return &Client{
		bin:           "tmux",
		enterWait:     250 * time.Millisecond,
		launchCommand: defaultLaunchCommand,
	}
}

func (c *Client) EnsureSession(ctx context.Context, spec SessionSpec) (bool, error) {
	if err := validateWorkingDirectory(spec.CWD); err != nil {
		return false, err
	}

	ok, err := c.hasSession(ctx, spec.SessionName)
	if err != nil {
		return false, err
	}

	created := false
	if ok {
		if err := c.ensureControlPane(ctx, spec); err != nil {
			return false, err
		}
	} else {
		if err := c.createSession(ctx, spec); err != nil {
			return false, err
		}
		created = true
	}

	wait := spec.StartupWait
	if wait <= 0 {
		wait = 4 * time.Second
	}

	timeout := wait
	if strings.TrimSpace(spec.LaunchCommand) != "" {
		if timeout < dockerReadyWait {
			timeout = dockerReadyWait
		}
	} else if timeout < hostReadyWait {
		timeout = hostReadyWait
	}
	if err := c.waitForPrompt(ctx, spec, timeout); err != nil {
		return false, err
	}

	return created, nil
}

func (c *Client) waitForPrompt(ctx context.Context, spec SessionSpec, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	lastTrustEnter := time.Time{}
	for {
		if ok, err := c.hasSession(ctx, spec.SessionName); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("tmux session %s exited before Codex became ready", spec.SessionName)
		}

		snapshot, err := c.Capture(ctx, spec.SessionName, 120)
		if err == nil {
			if spec.AutoPressEnterOnTrustPrompt && IsTrustPrompt(snapshot) && time.Since(lastTrustEnter) >= time.Second {
				if err := c.sendKey(ctx, spec.SessionName, "Enter"); err != nil {
					return err
				}
				lastTrustEnter = time.Now()
			}
			if _, hasPrompt := InputStatusSlot(snapshot); hasPrompt {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("tmux session %s did not become ready within %s", spec.SessionName, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollEvery):
		}
	}
}

func (c *Client) SendText(ctx context.Context, session string, text string) error {
	bufferName := "imcodex-" + SanitizeName(session)
	bufferPath, err := c.writeBufferFile(text)
	if err != nil {
		return err
	}
	defer os.Remove(bufferPath)

	if err := c.run(ctx, "load-buffer", "-b", bufferName, bufferPath); err != nil {
		return fmt.Errorf("load tmux buffer: %w", err)
	}

	target, err := c.controlPaneTarget(ctx, session)
	if err != nil {
		return err
	}
	if err := c.run(ctx, "paste-buffer", "-p", "-d", "-b", bufferName, "-t", target); err != nil {
		return fmt.Errorf("paste tmux buffer: %w", err)
	}
	if c.enterWait > 0 {
		time.Sleep(c.enterWait)
	}
	if err := c.sendKey(ctx, session, "Enter"); err != nil {
		return err
	}
	return nil
}

func (c *Client) writeBufferFile(text string) (string, error) {
	file, err := os.CreateTemp("", "imcodex-buffer-*")
	if err != nil {
		return "", fmt.Errorf("create tmux buffer file: %w", err)
	}
	path := file.Name()
	defer file.Close()

	if _, err := file.WriteString(text); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("write tmux buffer file: %w", err)
	}
	return path, nil
}

func (c *Client) Capture(ctx context.Context, session string, history int) (string, error) {
	if history <= 0 {
		history = 200
	}
	target, err := c.controlPaneTarget(ctx, session)
	if err != nil {
		return "", err
	}

	out, err := c.output(ctx, "capture-pane", "-pJ", "-S", fmt.Sprintf("-%d", history), "-t", target)
	if err != nil {
		return "", fmt.Errorf("capture tmux pane: %w", err)
	}
	return out, nil
}

func (c *Client) Interrupt(ctx context.Context, session string) error {
	return c.sendKey(ctx, session, "Escape")
}

func (c *Client) ForceInterrupt(ctx context.Context, session string) error {
	return c.sendKey(ctx, session, "C-c")
}

func validateWorkingDirectory(cwd string) error {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return fmt.Errorf("working directory is empty")
	}

	info, err := os.Stat(cwd)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("working directory does not exist: %s", cwd)
		}
		return fmt.Errorf("stat working directory %s: %w", cwd, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working directory is not a directory: %s", cwd)
	}
	return nil
}

func (c *Client) hasSession(ctx context.Context, session string) (bool, error) {
	cmd := exec.CommandContext(ctx, c.bin, "has-session", "-t", session)
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *Client) sendKey(ctx context.Context, session string, key string) error {
	target, err := c.controlPaneTarget(ctx, session)
	if err != nil {
		return err
	}
	if err := c.run(ctx, "send-keys", "-t", target, key); err != nil {
		return fmt.Errorf("send tmux key %s: %w", key, err)
	}
	return nil
}

func (c *Client) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c *Client) output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (c *Client) createSession(ctx context.Context, spec SessionSpec) error {
	paneID, err := c.output(ctx,
		"new-session", "-d", "-P", "-F", "#{pane_id}",
		"-s", spec.SessionName,
		"-n", controlWindowName,
		c.command(spec),
	)
	if err != nil {
		return fmt.Errorf("create tmux session: %w", err)
	}
	return c.setControlPane(ctx, spec.SessionName, strings.TrimSpace(paneID))
}

func (c *Client) ensureControlPane(ctx context.Context, spec SessionSpec) error {
	if _, err := c.controlPaneTarget(ctx, spec.SessionName); err == nil {
		return nil
	}

	paneID, err := c.output(ctx,
		"new-window", "-d", "-P", "-F", "#{pane_id}",
		"-t", spec.SessionName,
		"-n", controlWindowName,
		c.command(spec),
	)
	if err != nil {
		paneID, err = c.output(ctx,
			"new-window", "-d", "-P", "-F", "#{pane_id}",
			"-t", spec.SessionName,
			c.command(spec),
		)
		if err != nil {
			return fmt.Errorf("create tmux control pane: %w", err)
		}
	}
	return c.setControlPane(ctx, spec.SessionName, strings.TrimSpace(paneID))
}

func (c *Client) controlPaneTarget(ctx context.Context, session string) (string, error) {
	paneID, err := c.storedControlPane(ctx, session)
	if err != nil {
		return "", err
	}
	if paneID != "" {
		ok, err := c.hasPane(ctx, paneID)
		if err != nil {
			return "", err
		}
		if ok {
			return paneID, nil
		}
		paneID, err = c.findExistingControlPane(ctx, session, false)
	} else {
		paneID, err = c.findExistingControlPane(ctx, session, true)
	}
	if err != nil {
		return "", err
	}
	if paneID == "" {
		return "", fmt.Errorf("find tmux control pane in session %s: not found", session)
	}
	if err := c.setControlPane(ctx, session, paneID); err != nil {
		return "", err
	}
	return paneID, nil
}

func (c *Client) storedControlPane(ctx context.Context, session string) (string, error) {
	out, err := c.output(ctx, "show-options", "-qv", "-t", session, controlPaneOption)
	if err != nil {
		return "", fmt.Errorf("read tmux control pane option: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (c *Client) setControlPane(ctx context.Context, session string, paneID string) error {
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		return fmt.Errorf("set tmux control pane: empty pane id")
	}
	if err := c.run(ctx, "set-option", "-q", "-t", session, controlPaneOption, paneID); err != nil {
		return fmt.Errorf("set tmux control pane option: %w", err)
	}
	if err := c.run(ctx, "set-option", "-p", "-t", paneID, controlPaneRole, "control"); err != nil {
		return fmt.Errorf("set tmux control pane marker: %w", err)
	}
	return nil
}

func (c *Client) hasPane(ctx context.Context, paneID string) (bool, error) {
	out, err := c.output(ctx, "display-message", "-p", "-t", paneID, "#{pane_id}")
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "can't find pane"),
			strings.Contains(err.Error(), "can't find window"),
			strings.Contains(err.Error(), "can't find session"):
			return false, nil
		default:
			return false, fmt.Errorf("check tmux pane %s: %w", paneID, err)
		}
	}
	return strings.TrimSpace(out) == paneID, nil
}

func (c *Client) findExistingControlPane(ctx context.Context, session string, allowSinglePaneFallback bool) (string, error) {
	out, err := c.output(ctx, "list-panes", "-t", session, "-F", "#{pane_id}\t#{"+controlPaneRole+"}\t#{pane_current_command}")
	if err != nil {
		return "", fmt.Errorf("list tmux panes: %w", err)
	}

	type paneInfo struct {
		id      string
		role    string
		command string
	}

	var panes []paneInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		pane := paneInfo{id: strings.TrimSpace(parts[0])}
		if len(parts) > 1 {
			pane.role = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			pane.command = strings.TrimSpace(parts[2])
		}
		if pane.id != "" {
			panes = append(panes, pane)
		}
	}

	for _, pane := range panes {
		if pane.role == "control" {
			return pane.id, nil
		}
	}
	for _, pane := range panes {
		if pane.command == "codex" {
			return pane.id, nil
		}
	}
	if allowSinglePaneFallback && len(panes) == 1 {
		return panes[0].id, nil
	}
	return "", nil
}

func (c *Client) command(spec SessionSpec) string {
	if strings.TrimSpace(spec.LaunchCommand) != "" {
		return expandLaunchCommandTemplate(spec.LaunchCommand, spec)
	}
	if c.launchCommand != nil {
		return c.launchCommand(spec)
	}
	return defaultLaunchCommand(spec)
}

func defaultLaunchCommand(spec SessionSpec) string {
	return "exec " + shellJoin(
		"codex",
		"-a", "never",
		"-s", "danger-full-access",
		"--no-alt-screen",
		"-C", spec.CWD,
	)
}

func expandLaunchCommandTemplate(template string, spec SessionSpec) string {
	replacer := strings.NewReplacer(
		"{cwd}", spec.CWD,
		"{session_name}", spec.SessionName,
		"{group_id}", spec.GroupID,
		"{job_name}", spec.JobName,
	)
	return replacer.Replace(strings.TrimSpace(template))
}

func shellJoin(args ...string) string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		out = append(out, shellQuote(arg))
	}
	return strings.Join(out, " ")
}

func shellQuote(in string) string {
	if in == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(in, "'", `'\''`) + "'"
}
