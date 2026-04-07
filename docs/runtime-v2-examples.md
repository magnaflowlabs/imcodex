# Runtime 2.0 Examples

This page provides concrete example assets for the 2.0 runtime model.

## Included Files

- host wrapper: `tools/runtime/imcodex-agent-run`
- Codex image example: `tools/runtime/Dockerfile.codex`
- Claude image example: `tools/runtime/Dockerfile.claude`

## Build Example Images

```bash
docker build --build-arg CODEX_VERSION=0.118.0 -t imcodex-agent-codex:0.118.0 -f tools/runtime/Dockerfile.codex .
docker build --build-arg CLAUDE_CODE_VERSION=2.1.92 -t imcodex-agent-claude:2.1.92 -f tools/runtime/Dockerfile.claude .
```

## Example Group Config For Codex

```yaml
runtime: docker-codex

groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
```

## Example Group Config For Claude

```yaml
runtime: docker-claude

groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
```

## Example With Optional Config Seed

```yaml
runtime: docker-codex
runtime_config_dir: ~/.codex

groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
```

## Example Escape Hatch

```yaml
session_command: /srv/imcodex/tools/runtime/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent codex --config-dir '/home/ubuntu/.codex'

groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
```

## Notes

- The wrapper mounts only the configured workspace into `/workspace`.
- No host home directory is mounted by default.
- `runtime_config_dir` is optional and only seeds the container-local agent home.
- The example wrapper runs one disposable container per tmux-backed agent
  session.
- If you need persisted agent credentials, mount or inject them explicitly in
  the wrapper instead of mounting the host home directory wholesale.
- The example images pin CLI versions instead of using `latest`.
- The Claude example image sets `DISABLE_AUTOUPDATER=1` so startup is not
  interrupted by auto-update behavior.
- For Codex, prefer rebuilding the image on a maintenance window instead of
  installing `@latest` in a live runtime.
- If you omit both `runtime` and `session_command`, `imcodex` falls back to
  the legacy host-side Codex launch path for 1.x compatibility.
