# imcodex

`imcodex` bridges a Lark, Feishu, or Telegram group chat to long-lived Codex
sessions running in `tmux`.

## Runtime Model

`v2.2` supports two Codex runtimes:

- `docker-codex`
  This is the default and recommended production mode.
- `host-codex`
  This is opt-in and intended for manual debugging only.

`docker-codex` no longer needs `runtime`, `runtime_config_dir`, or
`session_command` in YAML. `imcodex` now manages the Docker launcher
internally and auto-builds the local `stable` image when needed.

## Requirements

Always required:

- `tmux`
- a Lark / Feishu bot or a Telegram bot

Required for the default `docker-codex` runtime:

- `docker`
- the runtime user must be able to run `docker` without `sudo`

Required only for `--runtime host-codex`:

- `nodejs`
- `npm`
- `@openai/codex`
- `bubblewrap`

## Install On Ubuntu

Base install for the recommended Docker runtime:

```bash
sudo apt update
sudo apt install -y tmux docker.io bubblewrap
sudo usermod -aG docker "$USER"
```

Log out and log back in so the `docker` group change takes effect, then verify:

```bash
tmux -V
docker --version
docker run --rm hello-world
```

If you want the optional host runtime as well:

```bash
sudo apt install -y nodejs npm
sudo npm install -g @openai/codex
codex --version
codex login
```

## Build

```bash
go build -o imcodex .
```

## Configuration

If `-config` is omitted, `imcodex` looks for config files in this order:

1. `./imcodex.yaml`
2. `~/.imcodex.yaml`

See [config.example.yaml](config.example.yaml).

Key fields:

| Field | Meaning |
| --- | --- |
| `platform` | `lark` or `telegram` |
| `interrupt_on_new_message` | If `true`, a new group message interrupts the current main-session run and keeps only the newest pending message |
| `groups[].group_id` | Group ID or Telegram chat ID |
| `groups[].cwd` | Working directory mapped to that group |
| `groups[].session_name` | Optional override for the tmux session name |
| `groups[].jobs[].name` | Job name shown in job result posts |
| `groups[].jobs[].schedule` | Standard 5-field cron expression |
| `groups[].jobs[].prompt_file` | Markdown prompt file for agent-driven jobs; relative paths resolve from the config file directory |
| `groups[].jobs[].command` | Shell command for deterministic CLI jobs; runs in `cwd` |
| `groups[].jobs[].artifacts_dir` | Optional base dir for per-run logs; relative paths resolve from `cwd` |
| `groups[].jobs[].summary_file` | Optional file whose content is posted on success; relative paths resolve from `cwd` |
| `groups[].jobs[].session_name` | Optional override for a `prompt_file` job session name |

Each job must set exactly one of `prompt_file` or `command`.

Path fields support:

- absolute paths
- `~/...`
- `$HOME/...`
- `${HOME}/...`

## Run

Recommended Docker runtime:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml
```

Explicit Docker runtime:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --runtime docker-codex
```

Docker runtime with a non-default Codex config directory:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --codex-config-dir ~/.codex
```

Optional host runtime:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --runtime host-codex
```

## Docker Runtime Behavior

When `imcodex` runs in `docker-codex` mode:

- it uses the current `imcodex` binary itself as the tmux pane launcher
- it ensures a local image tagged `imcodex-codex:stable` exists
- if the image is missing or stale, it rebuilds it automatically
- it mounts only the configured group `cwd` into the container as `/workspace`
- it copies the host Codex config directory into container-local `/home/agent/.codex`
- it launches Codex inside the container with:

```bash
codex -a never -s danger-full-access --no-alt-screen -C /workspace
```

The pinned Docker Codex CLI version for `v2.2.0` is `0.118.0`.

If you want to prebuild the same image manually:

```bash
docker build \
  --build-arg CODEX_VERSION=0.118.0 \
  --build-arg IMCODEX_IMAGE_REVISION=2.2.0 \
  -t imcodex-codex:stable \
  -f tools/runtime/Dockerfile.codex .
```

## Host Runtime Caveat

`host-codex` is kept for explicit manual use only.

It is not the recommended unattended mode because host-installed Codex may show
upgrade prompts that interrupt the session. If you need stable unattended
operation, use the default `docker-codex` runtime instead.

## Compatibility

`v2.2` removes these YAML fields:

- `runtime`
- `runtime_config_dir`
- `session_command`

If they still appear in config, `imcodex` now fails fast with a migration
error instead of silently mixing old and new runtime behavior.

Existing message routing, buffered Telegram output handling, scheduled jobs,
and `tmux` session reuse continue to work the same way.

## Runtime Docs

More detailed runtime notes:

- [docs/runtime-v2-docker-tmux.md](docs/runtime-v2-docker-tmux.md)
- [docs/runtime-v2-examples.md](docs/runtime-v2-examples.md)

## Example Startup Log

```text
imcodex 2.2.0 started: config=/srv/imcodex/imcodex.yaml platform=telegram runtime=docker-codex groups=1 jobs=1 base=https://api.telegram.org
```
