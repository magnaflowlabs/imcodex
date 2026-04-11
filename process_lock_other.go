//go:build !unix

package main

import "fmt"

func acquireProcessLock(configPath string) (func(), error) {
	return nil, fmt.Errorf("process locking is unsupported on this platform for config %s", configPath)
}
