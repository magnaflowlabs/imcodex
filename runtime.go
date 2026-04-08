package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	internalDockerCodexCommand = "internal-run-docker-codex"
	defaultDockerCodexImage    = "imcodex-codex:stable"
	defaultCodexConfigDirName  = ".codex"
	dockerCodexStableVersion   = "0.118.0"
	dockerCodexLabelVersion    = "com.magnaflowlabs.imcodex.codex-version"
	dockerCodexLabelRevision   = "com.magnaflowlabs.imcodex.image-revision"
	dockerAgentUser            = "agent"
	dockerAgentHome            = "/home/agent"
	dockerWorkspaceDir         = "/workspace"
)

var errDockerImageMissing = errors.New("docker image missing")

//go:embed tools/runtime/Dockerfile.codex
var dockerCodexDockerfile string

func maybeRunInternalCommand(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case internalDockerCodexCommand:
		return true, runInternalDockerCodex(args[1:])
	default:
		return false, nil
	}
}

func resolveLaunchCommand(runtime string, codexConfigDir string, executablePath string, lookupEnv func(string) (string, bool)) (string, string, error) {
	runtime, err := normalizeRuntime(runtime)
	if err != nil {
		return "", "", err
	}
	switch runtime {
	case runtimeHostCodex:
		return "", "", nil
	case runtimeDockerCodex:
		configDir, err := resolveDockerCodexConfigDir(codexConfigDir, lookupEnv)
		if err != nil {
			return "", "", err
		}
		executablePath, err = resolveExecutablePath(executablePath)
		if err != nil {
			return "", "", err
		}
		return buildInternalDockerLaunchCommand(executablePath, configDir), configDir, nil
	default:
		return "", "", fmt.Errorf("unsupported runtime: %s", runtime)
	}
}

func resolveDockerCodexConfigDir(value string, lookupEnv func(string) (string, bool)) (string, error) {
	if strings.TrimSpace(value) != "" {
		return resolveCLIPath(value, lookupEnv)
	}
	home := firstNonEmpty(envValue(lookupEnv, "HOME"), envValue(lookupEnv, "USERPROFILE"))
	if home == "" {
		return "", errors.New("docker-codex requires HOME or --codex-config-dir")
	}
	return filepath.Clean(filepath.Join(home, defaultCodexConfigDirName)), nil
}

func resolveExecutablePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("resolve executable path: empty path")
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve executable path %s: %w", value, err)
	}
	return filepath.Clean(abs), nil
}

func buildInternalDockerLaunchCommand(executablePath string, configDir string) string {
	return "exec " + shellJoin(
		executablePath,
		internalDockerCodexCommand,
		"--workspace", "{cwd}",
		"--session", "{session_name}",
		"--config-dir", configDir,
	)
}

func shellJoin(args ...string) string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		out = append(out, shellQuote(arg))
	}
	return strings.Join(out, " ")
}

func runInternalDockerCodex(args []string) error {
	fs := flag.NewFlagSet(internalDockerCodexCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var workspace string
	var session string
	var configDir string
	var image string
	fs.StringVar(&workspace, "workspace", "", "Workspace directory")
	fs.StringVar(&session, "session", "", "tmux session name")
	fs.StringVar(&configDir, "config-dir", "", "Codex config directory")
	fs.StringVar(&image, "image", defaultDockerCodexImage, "Docker image override")
	if err := fs.Parse(args); err != nil {
		return err
	}

	workspace, err := resolveExistingDir(workspace)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	configDir, err = resolveExistingDir(configDir)
	if err != nil {
		return fmt.Errorf("codex config dir: %w", err)
	}

	image = strings.TrimSpace(image)
	if image == "" {
		image = defaultDockerCodexImage
	}
	if err := ensureDockerCodexImage(image); err != nil {
		return err
	}

	dockerBin, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("docker not found: %w", err)
	}
	return syscall.Exec(dockerBin, append([]string{"docker"}, buildDockerRunArgs(image, workspace, session, configDir)...), os.Environ())
}

func resolveExistingDir(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(value) {
		abs, err := filepath.Abs(value)
		if err != nil {
			return "", err
		}
		value = abs
	}
	info, err := os.Stat(value)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", value)
	}
	return filepath.Clean(value), nil
}

func ensureDockerCodexImage(image string) error {
	version, revision, err := inspectDockerCodexImage(image)
	switch {
	case err == nil && version == dockerCodexStableVersion && revision == appVersion:
		return nil
	case err != nil && !errors.Is(err, errDockerImageMissing):
		return err
	}

	fmt.Fprintf(os.Stderr, "building %s with Codex CLI %s\n", image, dockerCodexStableVersion)

	buildDir, err := os.MkdirTemp("", "imcodex-docker-build-*")
	if err != nil {
		return fmt.Errorf("create docker build dir: %w", err)
	}
	defer os.RemoveAll(buildDir)

	cmd := exec.Command(
		"docker", "build",
		"--tag", image,
		"--build-arg", "CODEX_VERSION="+dockerCodexStableVersion,
		"--build-arg", "IMCODEX_IMAGE_REVISION="+appVersion,
		"-f", "-", ".",
	)
	cmd.Dir = buildDir
	cmd.Stdin = strings.NewReader(dockerCodexDockerfile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build docker image %s: %w", image, err)
	}
	return nil
}

func inspectDockerCodexImage(image string) (string, string, error) {
	cmd := exec.Command(
		"docker", "image", "inspect",
		"--format", "{{ index .Config.Labels \""+dockerCodexLabelVersion+"\" }}|{{ index .Config.Labels \""+dockerCodexLabelRevision+"\" }}",
		image,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "", "", errDockerImageMissing
		}
		return "", "", fmt.Errorf("inspect docker image %s: %w: %s", image, err, strings.TrimSpace(string(out)))
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 2)
	if len(parts) != 2 {
		return "", "", nil
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func buildDockerRunArgs(image string, workspace string, session string, configDir string) []string {
	containerName := dockerContainerName(session, workspace)
	args := []string{
		"run", "--rm", "-it",
		"--name", containerName,
		"--hostname", containerName,
		"--workdir", dockerWorkspaceDir,
		"--mount", fmt.Sprintf("type=bind,src=%s,dst=%s", workspace, dockerWorkspaceDir),
		"--tmpfs", "/tmp:exec,mode=1777",
		"-e", "HOME=" + dockerAgentHome,
		"-e", "CODEX_HOME=" + dockerAgentHome + "/.codex",
	}
	if configDir != "" {
		args = append(args, "--mount", fmt.Sprintf("type=bind,src=%s,dst=/config-ro,readonly", configDir))
	}
	args = append(args, image, "bash", "-lc", dockerCodexEntrypointScript())
	return args
}

func dockerCodexEntrypointScript() string {
	configHome := dockerAgentHome + "/.codex"
	return "set -euo pipefail; " +
		"mkdir -p " + shellQuote(configHome) + "; " +
		"if [[ -d /config-ro ]]; then cp -a /config-ro/. " + shellQuote(configHome) + "/; fi; " +
		"chown -R " + shellQuote(dockerAgentUser+":"+dockerAgentUser) + " " + shellQuote(dockerAgentHome) + "; " +
		"exec gosu " + shellQuote(dockerAgentUser) + " codex -a never -s danger-full-access --no-alt-screen -C " + shellQuote(dockerWorkspaceDir)
}

func dockerContainerName(session string, workspace string) string {
	containerName := "imcodex-" + sanitizeRuntimeToken(firstNonEmpty(session, workspace))
	if containerName == "imcodex-" {
		return "imcodex-agent"
	}
	return strings.Trim(containerName, "-")
}

func sanitizeRuntimeToken(in string) string {
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
	return strings.Trim(b.String(), "-")
}
