package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func acquireProcessLock(configPath string) (func(), error) {
	lockPath, err := processLockPath(configPath)
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("another imcodex process is already running for config %s", configPath)
		}
		return nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}

	_ = file.Truncate(0)
	_, _ = file.Seek(0, 0)
	_, _ = file.WriteString(fmt.Sprintf("%d\n", os.Getpid()))

	release := func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
	return release, nil
}

func processLockPath(configPath string) (string, error) {
	absPath, err := filepath.Abs(strings.TrimSpace(configPath))
	if err != nil {
		return "", fmt.Errorf("resolve config path %s: %w", configPath, err)
	}
	sum := sha256.Sum256([]byte(absPath))
	name := "imcodex-" + hex.EncodeToString(sum[:8]) + ".lock"
	return filepath.Join(os.TempDir(), name), nil
}
