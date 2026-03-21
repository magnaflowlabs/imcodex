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
	groupIDs := make([]string, 0, len(cfg.groups))
	for _, group := range cfg.groups {
		options = append(options, gateway.Options{
			GroupID:               group.GroupID,
			CWD:                   group.CWD,
			SessionName:           gateway.DefaultSessionNameForGroup(group.GroupID, group.CWD),
			InterruptOnNewMessage: cfg.interruptOnNewMessage,
		})
		groupIDs = append(groupIDs, group.GroupID)
	}

	larkClient := lark.NewClient(cfg.larkAppID, cfg.larkAppSecret, cfg.larkBaseURL)
	router, err := gateway.NewRouter(ctx, options, larkClient, tmuxctl.New(), larkClient, nil)
	if err != nil {
		log.Fatal(err)
	}

	receiver := lark.NewReceiver(cfg.larkAppID, cfg.larkAppSecret, cfg.larkBaseURL, router)
	poller := lark.NewPoller(larkClient, groupIDs, router, nil)
	log.Printf("imcodex started: config=%s groups=%d base=%s", cfg.path, router.GroupCount(), cfg.larkBaseURL)

	go func() {
		runReceiverLoop(ctx, receiver)
	}()
	go func() {
		if err := poller.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("WARN lark poller stopped: %v", err)
		}
	}()

	<-ctx.Done()
}

func runReceiverLoop(ctx context.Context, receiver *lark.Receiver) {
	for {
		err := receiver.Start(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		log.Printf("WARN lark long connection failed: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}
