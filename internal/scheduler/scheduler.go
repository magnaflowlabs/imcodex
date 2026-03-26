package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	GroupID     string
	CWD         string
	Name        string
	Schedule    string
	PromptFile  string
	SessionName string
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
		j.sendBestEffort(fmt.Sprintf("[job:%s] failed: %v", j.job.Name, err))
	}
}

func (j *jobRunner) run(ctx context.Context) error {
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
				j.sendBestEffort(fmt.Sprintf("[job:%s] completed with no visible output.", j.job.Name))
				return nil
			}
			j.sendChunked(fmt.Sprintf("[job:%s]\n%s", j.job.Name, latest))
			return nil
		}
	}
}

func normalizeJob(job Job) Job {
	job.GroupID = strings.TrimSpace(job.GroupID)
	job.CWD = strings.TrimSpace(job.CWD)
	job.Name = strings.TrimSpace(job.Name)
	job.Schedule = strings.TrimSpace(job.Schedule)
	job.PromptFile = strings.TrimSpace(job.PromptFile)
	job.SessionName = strings.TrimSpace(job.SessionName)
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
	case job.PromptFile == "":
		return fmt.Errorf("scheduled job %s prompt_file is required", job.Name)
	}
	if _, err := cron.ParseStandard(job.Schedule); err != nil {
		return fmt.Errorf("invalid schedule for job %s: %w", job.Name, err)
	}
	return nil
}

func DefaultSessionName(groupID string, cwd string, jobName string) string {
	return "imcodex-job-" + sanitizeName(filepath.Base(strings.TrimSpace(cwd))) + "-" + sanitizeName(groupID) + "-" + sanitizeName(jobName)
}

func sanitizeName(in string) string {
	var b strings.Builder
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "session"
	}
	return out
}

func (j *jobRunner) sendChunked(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	for _, chunk := range splitByRunes(text, maxMessageRunes) {
		j.sendBestEffort(chunk)
	}
}

func (j *jobRunner) sendBestEffort(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if err := j.messenger.SendTextToChat(context.Background(), j.job.GroupID, text); err != nil {
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
