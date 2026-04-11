# Runtime v2.2 Examples

## Default Host Runtime

Start `imcodex` with the default host runtime:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml
```

Equivalent explicit form:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --runtime host-codex
```

## Optional Docker Runtime

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --runtime docker-codex
```

## Docker Runtime With Custom Codex Config Dir

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --runtime docker-codex --codex-config-dir ~/.codex
```

`imcodex` copies that directory into container-local `/home/agent/.codex`
before launching Codex.

## Docker Runtime With A Custom Prebuilt Image

Add this to YAML:

```yaml
docker_image: ghcr.io/acme/imcodex-go:1.24
```

When `docker_image` is set, `imcodex` uses that image directly instead of
rebuilding the managed local `imcodex-codex:stable` image.

## Manual Stable Image Prebuild

`docker-codex` auto-builds this image when missing, but you can prebuild it:

```bash
docker build \
  --build-arg CODEX_VERSION=0.118.0 \
  --build-arg IMCODEX_IMAGE_REVISION=2.2.4 \
  -t imcodex-codex:stable \
  -f tools/runtime/Dockerfile.codex .
```

## Config Example

```yaml
platform: telegram
telegram_bot_token: 123456:ABCDEF
docker_image: ghcr.io/acme/imcodex-go:1.24
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
- `host-codex` is the default runtime in `v2.2.4`.
- `docker-codex` only activates when you pass `--runtime docker-codex`.
- `docker_image` is optional and only affects `docker-codex`.
- `~/...`, `$HOME/...`, and `${HOME}/...` work in path fields.
