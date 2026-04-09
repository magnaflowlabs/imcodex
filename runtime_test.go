package main

import (
	"strings"
	"testing"
)

func TestResolveLaunchCommandBuildsInternalDockerCommand(t *testing.T) {
	t.Parallel()

	command, configDir, err := resolveLaunchCommand(
		runtimeDockerCodex,
		"",
		"",
		"/srv/imcodex/imcodex",
		envLookup(map[string]string{"HOME": "/home/demo"}),
	)
	if err != nil {
		t.Fatalf("resolveLaunchCommand() error = %v", err)
	}
	if got, want := configDir, "/home/demo/.codex"; got != want {
		t.Fatalf("configDir = %q, want %q", got, want)
	}
	if !strings.Contains(command, "'internal-run-docker-codex'") {
		t.Fatalf("command = %q, want internal Docker launcher", command)
	}
	if !strings.Contains(command, "'--config-dir' '/home/demo/.codex'") {
		t.Fatalf("command = %q, want codex config dir", command)
	}
}

func TestResolveLaunchCommandLeavesHostCodexUnwrapped(t *testing.T) {
	t.Parallel()

	command, configDir, err := resolveLaunchCommand(
		runtimeHostCodex,
		"",
		"",
		"/srv/imcodex/imcodex",
		envLookup(map[string]string{"HOME": "/home/demo"}),
	)
	if err != nil {
		t.Fatalf("resolveLaunchCommand() error = %v", err)
	}
	if command != "" {
		t.Fatalf("command = %q, want empty host launch command", command)
	}
	if configDir != "" {
		t.Fatalf("configDir = %q, want empty host config dir", configDir)
	}
}

func TestResolveLaunchCommandUsesExplicitConfigDir(t *testing.T) {
	t.Parallel()

	command, configDir, err := resolveLaunchCommand(
		runtimeDockerCodex,
		"/srv/codex-config",
		"",
		"/srv/imcodex/imcodex",
		envLookup(map[string]string{"HOME": "/home/demo"}),
	)
	if err != nil {
		t.Fatalf("resolveLaunchCommand() error = %v", err)
	}
	if got, want := configDir, "/srv/codex-config"; got != want {
		t.Fatalf("configDir = %q, want %q", got, want)
	}
	if !strings.Contains(command, "'--config-dir' '/srv/codex-config'") {
		t.Fatalf("command = %q, want explicit config dir", command)
	}
}

func TestResolveLaunchCommandUsesCustomDockerImage(t *testing.T) {
	t.Parallel()

	command, configDir, err := resolveLaunchCommand(
		runtimeDockerCodex,
		"",
		"ghcr.io/acme/imcodex-go:1.24",
		"/srv/imcodex/imcodex",
		envLookup(map[string]string{"HOME": "/home/demo"}),
	)
	if err != nil {
		t.Fatalf("resolveLaunchCommand() error = %v", err)
	}
	if got, want := configDir, "/home/demo/.codex"; got != want {
		t.Fatalf("configDir = %q, want %q", got, want)
	}
	if !strings.Contains(command, "'--image' 'ghcr.io/acme/imcodex-go:1.24'") {
		t.Fatalf("command = %q, want custom docker image", command)
	}
}

func TestDockerContainerNamePrefersSession(t *testing.T) {
	t.Parallel()

	if got, want := dockerContainerName("Demo Session", "/srv/demo"), "imcodex-demo-session"; got != want {
		t.Fatalf("dockerContainerName() = %q, want %q", got, want)
	}
}

func TestDockerCodexEntrypointScriptCopiesConfigAndDropsPrivileges(t *testing.T) {
	t.Parallel()

	script := dockerCodexEntrypointScript()
	for _, want := range []string{
		"cp -a /config-ro/.",
		"chown -R 'agent:agent' '/home/agent'",
		"exec gosu 'agent' codex -a never -s danger-full-access --no-alt-screen -C '/workspace'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("entrypoint = %q, want substring %q", script, want)
		}
	}
}

func TestShouldEnsureDockerCodexImage(t *testing.T) {
	t.Parallel()

	if !shouldEnsureDockerCodexImage("") {
		t.Fatal("shouldEnsureDockerCodexImage(\"\") = false, want true")
	}
	if !shouldEnsureDockerCodexImage(defaultDockerCodexImage) {
		t.Fatalf("shouldEnsureDockerCodexImage(%q) = false, want true", defaultDockerCodexImage)
	}
	if shouldEnsureDockerCodexImage("ghcr.io/acme/imcodex-go:1.24") {
		t.Fatal("shouldEnsureDockerCodexImage(custom) = true, want false")
	}
}
