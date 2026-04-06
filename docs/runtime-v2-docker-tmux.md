# Runtime 2.0: Host tmux + Docker Workspace Sandbox

## Goal

This document defines the `imcodex` 2.0 runtime model for running either
Codex or Claude Code while keeping agent execution confined to a single
project workspace.

Primary requirements:

- `imcodex` must continue to drive one long-lived interactive agent session
  per group.
- The agent must not be able to scan the host filesystem outside the target
  working directory.
- Operators should still be able to attach to a readable terminal session for
  inspection and debugging.

## Compatibility

`imcodex` 2.0 must not remove the 1.x behavior.

Compatibility rules:

- Existing 1.x configs continue to work unchanged.
- If no new runtime/session fields are configured, `imcodex` keeps the
  current host-side Codex launch path.
- 2.0 adds an optional runtime override layer rather than replacing the
  existing defaults.

## Recommendation

2.0 should keep `tmux`, but move the agent process into Docker.

Recommended topology:

- `imcodex` runs on the host.
- `tmux` runs on the host.
- The pane command launched by `tmux` starts the agent inside Docker.
- Only the target `cwd` is mounted into the container.
- `imcodex` keeps using the existing tmux send/capture flow.

This preserves the current interaction model while moving agent execution out
of the host filesystem.

## Why This Is The Minimum-Change Path

`imcodex` is currently built around a long-lived interactive terminal session:

- it starts the agent inside `tmux`
- it pastes prompts into the pane
- it polls pane output and derives busy/idle state from the terminal stream
- it reuses sessions across restarts

Removing `tmux` would require replacing the execution layer with a structured
subprocess or SDK transport and rewriting the output-state model. That is a
valid future direction, but it is not a small change.

Keeping `tmux` on the host and running the agent inside Docker keeps the
existing contract intact:

- the terminal remains inspectable with `tmux attach`
- `imcodex` still reads one pane and writes one pane
- Docker becomes the isolation boundary

## Why tmux Should Stay On The Host

Do not put `tmux` inside the container for V1.

Host `tmux` is preferred because:

- `imcodex` already knows how to talk to host `tmux`
- operators can inspect sessions with the normal `tmux` commands
- the pane output is the actual agent TUI, not an extra nested terminal layer
- session lifecycle stays owned by `imcodex`, not by container internals

The container should run only the agent process and its supporting tools.

## Isolation Model

The agent should see exactly one writable project tree plus its own ephemeral
runtime directories.

Recommended container properties:

- bind-mount only the selected group `cwd` into the container
- set container working directory to that mount, for example `/workspace`
- do not mount host `$HOME`
- do not mount parent directories of the workspace
- do not mount the host Docker socket
- provide a container-local home directory such as `/home/agent`
- pass only the minimal credentials required by the chosen agent

This keeps the agent focused on the project while avoiding host-wide disk
visibility.

## Docker Runtime Shape

2.0 should support plain Docker first. Docker Sandboxes may be supported later
as an optional backend.

Why plain Docker first:

- works on normal Linux hosts
- avoids depending on Docker Desktop-only sandbox features
- easier to operate on the same class of servers where `imcodex` already runs

Why Docker Sandboxes may come later:

- stronger isolation model
- cleaner story for "YOLO inside sandbox, not on host"
- built-in agent templates for both Codex and Claude Code

## Agent Launch Shape

For 2.0, `imcodex` should not learn all Docker and agent details directly.
Instead, it should gain one configurable session launch command and leave the
container orchestration to an external wrapper script.

Recommended contract:

- `imcodex` expands a command template
- the command receives `cwd` and session metadata
- the wrapper script ensures the container or sandbox exists
- the wrapper script finally `exec`s the agent CLI in the container

Example responsibility split:

- `imcodex`: message routing, tmux session lifecycle, output buffering
- wrapper script: Docker image choice, environment injection, auth setup,
  agent selection, CLI flags

## Recommended 2.0 Config Surface

The minimum useful change is:

- optional per-group `session_name`
- optional per-group `session_command`
- optional per-job `session_name`
- optional per-job `session_command` for `prompt_file` jobs

Example shape:

```yaml
groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
    session_name: imcodex-my-project
    session_command: /usr/local/bin/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent codex
    jobs:
      - name: claude_review
        schedule: "5 * * * *"
        prompt_file: ./prompts/review.md
        session_name: imcodex-job-my-project-claude-review
        session_command: /usr/local/bin/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent claude
```

Template variables should stay small and explicit:

- `{cwd}`
- `{session_name}`
- `{group_id}`

If `session_command` is omitted, the current built-in Codex launch behavior
should remain the default.

The same fallback rule should apply at the job level.

## Wrapper Script Responsibilities

The wrapper script should:

1. Validate that the workspace exists.
2. Derive a stable container name from session or workspace.
3. Start or reuse the container runtime.
4. Mount only the workspace into the container.
5. Inject only agent-specific credentials and config.
6. `exec` either Codex or Claude Code in the container.

For Codex, the wrapper can safely use a fully autonomous mode inside the
container because Docker is the outer isolation boundary.

For Claude Code, the same principle applies: permissive execution is acceptable
only inside the sandboxed container, not on the host.

## Host tmux + Containerized Agent Flow

The runtime sequence should be:

1. `imcodex` asks host `tmux` to create or reuse a session.
2. The pane command runs the wrapper script.
3. The wrapper launches the agent in Docker with the workspace mounted as
   `/workspace`.
4. The agent TUI renders in the host `tmux` pane.
5. `imcodex` pastes prompts into the pane as it does today.
6. `imcodex` captures pane output as it does today.

This gives both properties the project wants:

- the operator still has a clean interactive terminal
- the agent is filesystem-confined to the workspace

## Required imcodex Changes

2.0 should stay small. The required code changes are:

1. Add optional per-group and per-job session launch command overrides.
2. Add optional per-group and per-job session name overrides.
3. Pass `cwd` and session metadata into the command template.
4. Relax control-pane recovery so it does not depend only on
   `pane_current_command == "codex"`.

Item 4 matters because once the pane command becomes a wrapper such as
`imcodex-agent-run` or `docker`, tmux will no longer report the current
command as `codex`.

Preferred recovery behavior:

- first trust the stored pane id
- if missing, look for a known pane marker owned by `imcodex`
- only then fall back to command-name heuristics

## What 2.0 Explicitly Does Not Do

2.0 does not attempt to:

- remove `tmux`
- replace pane scraping with a structured SDK stream
- support both Docker and Docker Sandboxes through a large runtime matrix
- expose every agent CLI flag in YAML

Those can be added later if the basic containerized session model proves
stable.

## Operational Notes

For Codex:

- prefer passing model and reasoning explicitly in the wrapper
- do not rely on host `~/.codex/config.toml` being mounted into the container

For Claude Code:

- prefer API-key based auth in the container
- keep any Claude config inside the container-local home directory

For both:

- mount only one workspace
- keep runtime temp files inside the container
- avoid mounting secrets by default beyond the one agent credential needed

## Sample Wrapper Behavior

The wrapper script is the right place to choose between Codex and Claude.

Codex example responsibilities:

- ensure the Codex image is present
- mount `{cwd}` at `/workspace`
- run `codex --no-alt-screen -C /workspace ...`
- pass model/reasoning flags explicitly if desired

Claude example responsibilities:

- ensure the Claude image is present
- mount `{cwd}` at `/workspace`
- run `claude` inside the same terminal session
- keep auth and config container-local

The wrapper should end with `exec ...` so the visible process inside the tmux
pane is the long-lived wrapper or agent process rather than a dead shell.

## Recommended 2.0 Decision

Use this as the first implementation target:

- host `imcodex`
- host `tmux`
- one Docker-backed agent session per group
- one mounted workspace per session
- wrapper-script launch override

This is the cleanest way to combine:

- workspace-only disk visibility
- existing `imcodex` session logic
- readable interactive terminal sessions

## References

- Docker Sandboxes overview:
  https://docs.docker.com/ai/sandboxes/
- Docker supported agents:
  https://docs.docker.com/ai/sandboxes/agents/
- Docker Claude Code sandbox:
  https://docs.docker.com/ai/sandboxes/agents/claude-code/
- Docker Codex sandbox:
  https://docs.docker.com/ai/sandboxes/agents/codex/
- Claude Code SDK and CLI non-interactive mode:
  https://docs.anthropic.com/s/claude-code-sdk
