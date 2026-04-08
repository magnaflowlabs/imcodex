# Runtime v2.2: Host tmux + Docker Codex

## Summary

`v2.2` keeps `tmux` on the host and moves Codex into Docker by default.

Runtime selection is now a startup flag:

- default: `docker-codex`
- explicit host fallback: `--runtime host-codex`

YAML no longer controls runtime choice.

## Why This Changed

The older runtime surface had three practical problems:

1. too many ways to launch the agent
2. external wrapper drift
3. host Codex upgrade prompts interrupting unattended traffic

`v2.2` fixes that by making one path the default:

- one `imcodex` binary
- one embedded Docker launcher
- one local stable image tag: `imcodex-codex:stable`

## Runtime Flow

For `docker-codex`:

1. `imcodex` starts or reuses a host `tmux` session.
2. The tmux pane runs the same `imcodex` binary in an internal launcher mode.
3. The launcher ensures `imcodex-codex:stable` exists.
4. If needed, it builds the image from the embedded `tools/runtime/Dockerfile.codex`.
5. Docker starts Codex with only the configured group `cwd` mounted as `/workspace`.
6. The host Codex config directory is copied into container-local `/home/agent/.codex`.
7. Codex runs inside the container and the TUI remains visible in the host tmux pane.

For `host-codex`:

1. `imcodex` starts host `codex` directly in the tmux pane.
2. This mode is supported only when you explicitly pass `--runtime host-codex`.

## Isolation Model

The Docker runtime keeps the same operator workflow while narrowing filesystem
visibility:

- host `tmux`
- host `imcodex`
- containerized `codex`
- one mounted workspace
- no host home directory bind mount

Only the selected `cwd` is mounted into the container.

## Config Surface

YAML now contains only chat routing and job configuration.

Removed fields:

- `runtime`
- `runtime_config_dir`
- `session_command`

Replacement startup flags:

- `--runtime docker-codex|host-codex`
- `--codex-config-dir /path/to/.codex`

## Stable Codex Version

The Docker runtime for `v2.2.0` pins Codex CLI `0.118.0`.

That version is baked into the local `stable` image build. This avoids live
interactive upgrade prompts during production traffic.

## Operational Notes

- `docker-codex` is the recommended unattended mode.
- `host-codex` is kept only for explicit debugging or temporary local use.
- `imcodex` waits for the Docker-backed Codex prompt before sending chat text,
  so the first prompt is not pasted into an image build shell.
