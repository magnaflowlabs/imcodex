package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/magnaflowlabs/imcodex/internal/gateway"
	"github.com/magnaflowlabs/imcodex/internal/tmuxctl"
	"github.com/robfig/cron/v3"
)

const maxMessageRunes = 3000

type Job struct {
	GroupID       string
	CWD           string
	Name          string
	Schedule      string
	PromptFile    string
	Command       string
	ArtifactsDir  string
	SummaryFile   string
	SessionName   string
	LaunchCommand string
}

type Console interface {
	EnsureSession(ctx context.Context, spec tmuxctl.SessionSpec) (bool, error)
	SendText(ctx context.Context, session string, text string) error
	Capture(ctx context.Context, session string, history int) (string, error)
}

type Runner struct {
	cron *cron.Cron
	jobs []*jobRunner

	mu  sync.RWMutex
	ctx context.Context
}

type jobRunner struct {
	job       Job
	messenger gateway.Messenger
	console   Console
	logger    *slog.Logger

	pollEvery time.Duration
	startWait time.Duration
	history   int

	mu      sync.Mutex
	running bool
}

type commandRunFiles struct {
	RunID        string
	ArtifactsDir string
	SummaryFile  string
	StdoutFile   string
	StderrFile   string
	CombinedFile string
}

type reportedError struct {
	err error
}

func (e reportedError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e reportedError) Unwrap() error {
	return e.err
}

func New(jobs []Job, messenger gateway.Messenger, console Console, logger *slog.Logger) (*Runner, error) {
	if logger == nil {
		logger = slog.Default()
	}

	c := cron.New(cron.WithLocation(time.Local))
	runner := &Runner{cron: c}
	for _, job := range jobs {
		job = normalizeJob(job)
		if err := validateJob(job); err != nil {
			return nil, err
		}

		jr := &jobRunner{
			job:       job,
			messenger: messenger,
			console:   console,
			logger:    logger,
			pollEvery: 500 * time.Millisecond,
			startWait: 4 * time.Second,
			history:   2000,
		}
		if _, err := c.AddFunc(job.Schedule, func() { runner.runJob(jr) }); err != nil {
			return nil, fmt.Errorf("add scheduled job %s: %w", job.Name, err)
		}
		runner.jobs = append(runner.jobs, jr)
	}
	return runner, nil
}

func (r *Runner) Start(ctx context.Context) error {
	if r == nil || r.cron == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	r.cron.Start()
	<-ctx.Done()
	stopCtx := r.cron.Stop()
	select {
	case <-stopCtx.Done():
	case <-time.After(5 * time.Second):
	}
	return nil
}

func (r *Runner) JobCount() int {
	if r == nil {
		return 0
	}
	return len(r.jobs)
}

func (r *Runner) runJob(job *jobRunner) {
	r.mu.RLock()
	ctx := r.ctx
	r.mu.RUnlock()
	if ctx == nil {
		ctx = context.Background()
	}
	job.tryRun(ctx)
}

func (j *jobRunner) tryRun(ctx context.Context) {
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		j.logger.Warn("skip scheduled job because previous run is still active", "job", j.job.Name, "group_id", j.job.GroupID, "session", j.job.SessionName)
		return
	}
	j.running = true
	j.mu.Unlock()

	defer func() {
		j.mu.Lock()
		j.running = false
		j.mu.Unlock()
	}()

	if err := j.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		j.logger.Error("scheduled job failed", "job", j.job.Name, "group_id", j.job.GroupID, "session", j.job.SessionName, "err", err)
		var reported reportedError
		if !errors.As(err, &reported) {
			j.sendBestEffort(ctx, fmt.Sprintf("[job:%s] failed: %v", j.job.Name, err))
		}
	}
}

func (j *jobRunner) run(ctx context.Context) error {
	if j.job.Command != "" {
		return j.runCommand(ctx)
	}
	return j.runPrompt(ctx)
}

func (j *jobRunner) runPrompt(ctx context.Context) error {
	promptBytes, err := os.ReadFile(j.job.PromptFile)
	if err != nil {
		return fmt.Errorf("read prompt file %s: %w", j.job.PromptFile, err)
	}
	prompt := strings.TrimSpace(string(promptBytes))
	if prompt == "" {
		return fmt.Errorf("prompt file is empty: %s", j.job.PromptFile)
	}

	if _, err := j.console.EnsureSession(ctx, tmuxctl.SessionSpec{
		SessionName:                 j.job.SessionName,
		CWD:                         j.job.CWD,
		GroupID:                     j.job.GroupID,
		JobName:                     j.job.Name,
		LaunchCommand:               j.job.LaunchCommand,
		StartupWait:                 j.startWait,
		AutoPressEnterOnTrustPrompt: true,
	}); err != nil {
		return err
	}

	baseSnapshot, err := j.console.Capture(ctx, j.job.SessionName, j.history)
	if err != nil {
		return err
	}
	if tmuxctl.IsBusy(baseSnapshot) {
		return fmt.Errorf("job session is still busy from a previous run")
	}

	baseText := tmuxctl.NormalizeSnapshot(baseSnapshot)
	if err := j.console.SendText(ctx, j.job.SessionName, prompt); err != nil {
		return err
	}

	ticker := time.NewTicker(j.pollEvery)
	defer ticker.Stop()

	idleTicks := 0
	latest := ""
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			snapshot, err := j.console.Capture(ctx, j.job.SessionName, j.history)
			if err != nil {
				return err
			}

			latest = strings.TrimSpace(tmuxctl.SliceAfter(baseText, tmuxctl.NormalizeSnapshot(snapshot)))
			if tmuxctl.IsBusy(snapshot) {
				idleTicks = 0
				continue
			}

			idleTicks++
			if idleTicks < 2 {
				continue
			}
			if latest == "" {
				j.sendBestEffort(ctx, fmt.Sprintf("[job:%s] completed with no visible output.", j.job.Name))
				return nil
			}
			j.sendChunked(ctx, fmt.Sprintf("[job:%s]\n%s", j.job.Name, latest))
			return nil
		}
	}
}

func (j *jobRunner) runCommand(ctx context.Context) error {
	runFiles, err := j.prepareCommandRunFiles()
	if err != nil {
		return err
	}

	stdout, stderr, combined, err := j.executeCommand(ctx, runFiles)
	if writeErr := j.writeCommandOutputs(runFiles, stdout, stderr, combined); writeErr != nil {
		return writeErr
	}

	if err != nil {
		message := fmt.Sprintf("[job:%s] failed", j.job.Name)
		if stage := detectStageHint(combined); stage != "" {
			message += fmt.Sprintf(" during %s", stage)
		}
		message += fmt.Sprintf(": %v\nartifacts: %s", err, runFiles.ArtifactsDir)
		if tail := trimOutputTail(combined, 20); tail != "" {
			message += "\nlast output:\n" + tail
		}
		j.sendChunked(ctx, message)
		return reportedError{err: fmt.Errorf("command job %s: %w", j.job.Name, err)}
	}

	summary := readTrimmedFile(runFiles.SummaryFile)
	switch {
	case summary != "":
		j.sendChunked(ctx, fmt.Sprintf("[job:%s] completed.\nartifacts: %s\n%s", j.job.Name, runFiles.ArtifactsDir, summary))
	case strings.TrimSpace(combined) != "":
		j.sendChunked(ctx, fmt.Sprintf("[job:%s] completed.\nartifacts: %s\noutput:\n%s", j.job.Name, runFiles.ArtifactsDir, trimOutputTail(combined, 20)))
	default:
		j.sendBestEffort(ctx, fmt.Sprintf("[job:%s] completed.\nartifacts: %s", j.job.Name, runFiles.ArtifactsDir))
	}
	return nil
}

func (j *jobRunner) prepareCommandRunFiles() (commandRunFiles, error) {
	baseDir := strings.TrimSpace(j.job.ArtifactsDir)
	if baseDir == "" {
		baseDir = filepath.Join(j.job.CWD, ".imcodex", "jobs", tmuxctl.SanitizeName(j.job.Name))
	}

	runID := time.Now().UTC().Format("20060102T150405.000000000Z")
	runDir := filepath.Join(baseDir, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return commandRunFiles{}, fmt.Errorf("create artifacts dir %s: %w", runDir, err)
	}

	summaryFile := strings.TrimSpace(j.job.SummaryFile)
	if summaryFile == "" {
		summaryFile = filepath.Join(runDir, "summary.md")
	}
	if err := os.MkdirAll(filepath.Dir(summaryFile), 0o755); err != nil {
		return commandRunFiles{}, fmt.Errorf("create summary dir %s: %w", filepath.Dir(summaryFile), err)
	}
	if err := os.WriteFile(summaryFile, nil, 0o644); err != nil {
		return commandRunFiles{}, fmt.Errorf("reset summary file %s: %w", summaryFile, err)
	}

	return commandRunFiles{
		RunID:        runID,
		ArtifactsDir: runDir,
		SummaryFile:  summaryFile,
		StdoutFile:   filepath.Join(runDir, "stdout.log"),
		StderrFile:   filepath.Join(runDir, "stderr.log"),
		CombinedFile: filepath.Join(runDir, "combined.log"),
	}, nil
}

func (j *jobRunner) executeCommand(ctx context.Context, runFiles commandRunFiles) (string, string, string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-lc", j.job.Command)
	cmd.Dir = j.job.CWD
	cmd.Env = append(os.Environ(),
		"IMCODEX_JOB_NAME="+j.job.Name,
		"IMCODEX_JOB_GROUP_ID="+j.job.GroupID,
		"IMCODEX_JOB_RUN_ID="+runFiles.RunID,
		"IMCODEX_JOB_CWD="+j.job.CWD,
		"IMCODEX_JOB_ARTIFACTS_DIR="+runFiles.ArtifactsDir,
		"IMCODEX_JOB_SUMMARY_FILE="+runFiles.SummaryFile,
		"IMCODEX_JOB_STDOUT_FILE="+runFiles.StdoutFile,
		"IMCODEX_JOB_STDERR_FILE="+runFiles.StderrFile,
		"IMCODEX_JOB_COMBINED_FILE="+runFiles.CombinedFile,
	)

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	var combinedBuf lockedBuffer
	cmd.Stdout = multiBufferWriter(&combinedBuf, &stdoutBuf)
	cmd.Stderr = multiBufferWriter(&combinedBuf, &stderrBuf)
	err := cmd.Run()
	return stdoutBuf.String(), stderrBuf.String(), combinedBuf.String(), err
}

func (j *jobRunner) writeCommandOutputs(runFiles commandRunFiles, stdout string, stderr string, combined string) error {
	outputs := map[string]string{
		runFiles.StdoutFile:   stdout,
		runFiles.StderrFile:   stderr,
		runFiles.CombinedFile: combined,
	}
	for path, content := range outputs {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write job output %s: %w", path, err)
		}
	}
	return nil
}

func normalizeJob(job Job) Job {
	job.GroupID = strings.TrimSpace(job.GroupID)
	job.CWD = strings.TrimSpace(job.CWD)
	job.Name = strings.TrimSpace(job.Name)
	job.Schedule = strings.TrimSpace(job.Schedule)
	job.PromptFile = strings.TrimSpace(job.PromptFile)
	job.Command = strings.TrimSpace(job.Command)
	job.ArtifactsDir = strings.TrimSpace(job.ArtifactsDir)
	job.SummaryFile = strings.TrimSpace(job.SummaryFile)
	job.SessionName = strings.TrimSpace(job.SessionName)
	job.LaunchCommand = strings.TrimSpace(job.LaunchCommand)
	if job.SessionName == "" {
		job.SessionName = DefaultSessionName(job.GroupID, job.CWD, job.Name)
	}
	return job
}

func validateJob(job Job) error {
	switch {
	case job.GroupID == "":
		return errors.New("scheduled job group_id is required")
	case job.CWD == "":
		return fmt.Errorf("scheduled job %s cwd is required", job.Name)
	case job.Name == "":
		return errors.New("scheduled job name is required")
	case job.Schedule == "":
		return fmt.Errorf("scheduled job %s schedule is required", job.Name)
	case job.PromptFile != "" && job.Command != "":
		return fmt.Errorf("scheduled job %s must set only one of prompt_file or command", job.Name)
	case job.PromptFile == "" && job.Command == "":
		return fmt.Errorf("scheduled job %s must set one of prompt_file or command", job.Name)
	case job.Command == "" && job.ArtifactsDir != "":
		return fmt.Errorf("scheduled job %s artifacts_dir requires command", job.Name)
	case job.Command == "" && job.SummaryFile != "":
		return fmt.Errorf("scheduled job %s summary_file requires command", job.Name)
	}
	if _, err := cron.ParseStandard(job.Schedule); err != nil {
		return fmt.Errorf("invalid schedule for job %s: %w", job.Name, err)
	}
	return nil
}

func DefaultSessionName(groupID string, cwd string, jobName string) string {
	return "imcodex-job-" + tmuxctl.SanitizeName(filepath.Base(strings.TrimSpace(cwd))) + "-" + tmuxctl.SanitizeName(groupID) + "-" + tmuxctl.SanitizeName(jobName)
}

func (j *jobRunner) sendChunked(ctx context.Context, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	for _, chunk := range splitByRunes(text, maxMessageRunes) {
		j.sendBestEffort(ctx, chunk)
	}
}

func (j *jobRunner) sendBestEffort(ctx context.Context, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return
	}
	if err := j.messenger.SendTextToChat(ctx, j.job.GroupID, text); err != nil {
		j.logger.Error("send scheduled job message failed", "job", j.job.Name, "group_id", j.job.GroupID, "err", err)
	}
}

func splitByRunes(text string, limit int) []string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	var chunks []string
	var builder strings.Builder
	count := 0
	for _, r := range text {
		builder.WriteRune(r)
		count++
		if count >= limit {
			chunks = append(chunks, builder.String())
			builder.Reset()
			count = 0
		}
	}
	if builder.Len() > 0 {
		chunks = append(chunks, builder.String())
	}
	return chunks
}

func readTrimmedFile(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func trimOutputTail(text string, maxLines int) string {
	text = strings.TrimSpace(text)
	if text == "" || maxLines <= 0 {
		return text
	}

	lines := strings.Split(text, "\n")
	start := len(lines) - maxLines
	if start < 0 {
		start = 0
	}
	return strings.TrimSpace(strings.Join(lines[start:], "\n"))
}

func detectStageHint(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.Contains(line, "]") {
			return line
		}
	}
	return ""
}

func multiBufferWriter(writers ...io.Writer) *bufferGroup {
	return &bufferGroup{writers: writers}
}

type bufferGroup struct {
	mu      sync.Mutex
	writers []io.Writer
}

func (g *bufferGroup) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for _, writer := range g.writers {
		if writer == nil {
			continue
		}
		if _, err := writer.Write(p); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
