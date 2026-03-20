package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

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
	for _, group := range cfg.groups {
		options = append(options, gateway.Options{
			GroupID:     group.GroupID,
			CWD:         group.CWD,
			SessionName: gateway.DefaultSessionNameForGroup(group.GroupID, group.CWD),
		})
	}

	router, err := gateway.NewRouter(ctx, options, lark.NewClient(cfg.larkAppID, cfg.larkAppSecret, cfg.larkBaseURL), tmuxctl.New(), nil)
	if err != nil {
		log.Fatal(err)
	}

	receiver := lark.NewReceiver(cfg.larkAppID, cfg.larkAppSecret, cfg.larkBaseURL, router)
	log.Printf("imcodex started: config=%s groups=%d base=%s", cfg.path, router.GroupCount(), cfg.larkBaseURL)

	errCh := make(chan error, 1)
	go func() {
		errCh <- receiver.Start(ctx)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("lark long connection failed: %v", err)
		}
	case <-ctx.Done():
	}
}
