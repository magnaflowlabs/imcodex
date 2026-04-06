package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/magnaflowlabs/imcodex/internal/gateway"
	"github.com/magnaflowlabs/imcodex/internal/lark"
	"github.com/magnaflowlabs/imcodex/internal/scheduler"
	"github.com/magnaflowlabs/imcodex/internal/telegram"
	"github.com/magnaflowlabs/imcodex/internal/tmuxctl"
)

func main() {
	cfg, err := parseConfig(os.Args[1:], os.LookupEnv, os.ReadFile)
	if err != nil {
		log.Fatal(err)
	}

	releaseLock, err := acquireProcessLock(cfg.path)
	if err != nil {
		log.Fatal(err)
	}
	defer releaseLock()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	options := make([]gateway.Options, 0, len(cfg.groups))
	for _, group := range cfg.groups {
		options = append(options, gateway.Options{
			GroupID:               group.GroupID,
			CWD:                   group.CWD,
			SessionName:           firstNonEmpty(group.SessionName, gateway.DefaultSessionNameForGroup(group.GroupID, group.CWD)),
			SessionCommand:        group.SessionCommand,
			InterruptOnNewMessage: cfg.interruptOnNewMessage,
		})
	}

	console := tmuxctl.New()
	var (
		startFuncs []func(context.Context) error
		baseURL    string
	)

	router, err := buildRouter(ctx, cfg, options, console, &startFuncs, &baseURL)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf(
		"imcodex %s started: config=%s platform=%s groups=%d jobs=%d base=%s",
		appVersion,
		cfg.path,
		cfg.platform,
		router.GroupCount(),
		countJobs(cfg.groups),
		baseURL,
	)
	for _, start := range startFuncs {
		go func(start func(context.Context) error) {
			if err := start(ctx); err != nil && ctx.Err() == nil {
				log.Printf("WARN bridge loop stopped: %v", err)
			}
		}(start)
	}

	<-ctx.Done()
}

func buildRouter(ctx context.Context, cfg config, options []gateway.Options, console gateway.Console, startFuncs *[]func(context.Context) error, baseURL *string) (*gateway.Router, error) {
	switch cfg.platform {
	case "telegram":
		tgClient := telegram.NewClient(cfg.telegramBotToken, cfg.telegramBaseURL)
		router, err := gateway.NewRouter(ctx, options, tgClient, console, tgClient, nil)
		if err != nil {
			return nil, err
		}
		if runner, err := buildScheduler(cfg.groups, tgClient, console); err != nil {
			return nil, err
		} else if runner != nil {
			*startFuncs = append(*startFuncs, runner.Start)
		}
		receiver := telegram.NewReceiver(tgClient, router, nil)
		*baseURL = cfg.telegramBaseURL
		*startFuncs = append(*startFuncs, receiver.Start)
		return router, nil
	default:
		groupIDs := make([]string, 0, len(cfg.groups))
		for _, group := range cfg.groups {
			groupIDs = append(groupIDs, group.GroupID)
		}
		larkClient := lark.NewClient(cfg.larkAppID, cfg.larkAppSecret, cfg.larkBaseURL)
		router, err := gateway.NewRouter(ctx, options, larkClient, console, larkClient, nil)
		if err != nil {
			return nil, err
		}
		if runner, err := buildScheduler(cfg.groups, larkClient, console); err != nil {
			return nil, err
		} else if runner != nil {
			*startFuncs = append(*startFuncs, runner.Start)
		}
		receiver := lark.NewReceiver(cfg.larkAppID, cfg.larkAppSecret, cfg.larkBaseURL, router)
		poller := lark.NewPoller(larkClient, groupIDs, router, nil)
		*baseURL = cfg.larkBaseURL
		*startFuncs = append(*startFuncs,
			func(ctx context.Context) error { return runLarkReceiverLoop(ctx, receiver) },
			poller.Start,
		)
		return router, nil
	}
}

func buildScheduler(groups []groupConfig, messenger gateway.Messenger, console gateway.Console) (*scheduler.Runner, error) {
	jobs := buildScheduledJobs(groups)
	if len(jobs) == 0 {
		return nil, nil
	}
	return scheduler.New(jobs, messenger, console, nil)
}

func buildScheduledJobs(groups []groupConfig) []scheduler.Job {
	jobs := make([]scheduler.Job, 0)
	for _, group := range groups {
		for _, job := range group.Jobs {
			jobs = append(jobs, scheduler.Job{
				GroupID:        group.GroupID,
				CWD:            group.CWD,
				Name:           job.Name,
				Schedule:       job.Schedule,
				PromptFile:     job.PromptFile,
				Command:        job.Command,
				ArtifactsDir:   job.ArtifactsDir,
				SummaryFile:    job.SummaryFile,
				SessionName:    firstNonEmpty(job.SessionName, scheduler.DefaultSessionName(group.GroupID, group.CWD, job.Name)),
				SessionCommand: firstNonEmpty(job.SessionCommand, group.SessionCommand),
			})
		}
	}
	return jobs
}

func countJobs(groups []groupConfig) int {
	total := 0
	for _, group := range groups {
		total += len(group.Jobs)
	}
	return total
}

func runLarkReceiverLoop(ctx context.Context, receiver *lark.Receiver) error {
	for {
		err := receiver.Start(ctx)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		log.Printf("WARN lark long connection failed: %v", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}
