# Runtime 2.0 Examples

This page provides concrete example assets for the 2.0 runtime model.

## Included Files

- host wrapper: `tools/runtime/imcodex-agent-run`
- Codex image example: `tools/runtime/Dockerfile.codex`
- Claude image example: `tools/runtime/Dockerfile.claude`

## Build Example Images

```bash
docker build -t imcodex-agent-codex:latest -f tools/runtime/Dockerfile.codex .
docker build -t imcodex-agent-claude:latest -f tools/runtime/Dockerfile.claude .
```

## Example Group Config For Codex

```yaml
session_command: /srv/imcodex/tools/runtime/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent codex

groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
```

## Example Group Config For Claude

```yaml
session_command: /srv/imcodex/tools/runtime/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent claude

groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
```

## Example Job Override

```yaml
session_command: /srv/imcodex/tools/runtime/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent codex

groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
    jobs:
      - name: nightly_review
        schedule: "5 2 * * *"
        prompt_file: ./prompts/nightly_review.md
        session_name: imcodex-job-nightly-review
```

## Notes

- The wrapper mounts only the configured workspace into `/workspace`.
- No host home directory is mounted by default.
- The example wrapper runs one disposable container per tmux-backed agent
  session.
- If you need persisted agent credentials, mount or inject them explicitly in
  the wrapper instead of mounting the host home directory wholesale.
