# Runtime v2.2 Examples

## Recommended Docker Runtime

Start `imcodex` with the default Docker runtime:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml
```

Equivalent explicit form:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --runtime docker-codex
```

## Docker Runtime With Custom Codex Config Dir

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --codex-config-dir ~/.codex
```

`imcodex` copies that directory into container-local `/home/agent/.codex`
before launching Codex.

## Optional Host Runtime

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --runtime host-codex
```

Use this only when you deliberately want the host-installed Codex CLI.

## Manual Stable Image Prebuild

`docker-codex` auto-builds this image when missing, but you can prebuild it:

```bash
docker build \
  --build-arg CODEX_VERSION=0.118.0 \
  --build-arg IMCODEX_IMAGE_REVISION=2.2.0 \
  -t imcodex-codex:stable \
  -f tools/runtime/Dockerfile.codex .
```

## Config Example

```yaml
platform: telegram
telegram_bot_token: 123456:ABCDEF
interrupt_on_new_message: true

groups:
  - group_id: -1001234567890
    cwd: ~/work/my-project
    jobs:
      - name: hourly_review
        schedule: "1 * * * *"
        prompt_file: ./prompts/hourly_review.md
```

## Notes

- YAML no longer contains `runtime`, `runtime_config_dir`, or `session_command`.
- `docker-codex` is the default runtime in `v2.2`.
- `host-codex` only activates when you pass `--runtime host-codex`.
- `~/...`, `$HOME/...`, and `${HOME}/...` work in path fields.
