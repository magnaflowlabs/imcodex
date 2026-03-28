# Telegram Output Buffering

## Status

| Item | Value |
| --- | --- |
| Scope | Main chat replies forwarded from Codex to Telegram |
| Status | Implemented |
| Goal | Reduce fragmented Telegram messages without making the first visible response feel slow |

## Problem

| Symptom | Cause |
| --- | --- |
| One Codex answer appears as many Telegram messages | `imcodex` flushes too soon when the tmux snapshot briefly goes idle |
| Users feel the bot is slow if nothing appears quickly | Waiting for the full answer before posting removes early feedback |

## Decision

| Topic | Decision |
| --- | --- |
| First visible feedback | Send one short `working` status quickly |
| Telegram reply mode | Prefer `editMessageText` on one active Telegram message |
| Body flush | Use a longer debounce before forwarding reply text |
| Flush trigger | Flush body only after the session has been idle for a configurable window |
| Before next user message dispatch | Force-flush any buffered body text first |
| Telegram length limit | When the active message approaches a soft limit, roll over to a new Telegram message |

## Target Behavior

| Phase | Behavior |
| --- | --- |
| User sends prompt | Prompt is forwarded to Codex immediately |
| Codex is still thinking | After a short delay, send one status message such as `[working] Codex is processing.` and keep its Telegram `message_id` as the active output message |
| Codex starts producing content | Buffer reply text; do not send each short pause as a separate Telegram message |
| Codex becomes idle | Flush buffered reply text after the debounce window by editing the active Telegram message |
| Active message nears Telegram limit | Finalize the current message and create a new continuation message, then continue editing the new one |
| New user prompt arrives while previous reply text is buffered | Flush buffered text first, then dispatch the new prompt |

## Proposed Timing

| Parameter | Proposed default | Notes |
| --- | --- | --- |
| `working_after` | `1s` | Fast feedback so the group knows the bot is alive |
| `body_flush_after` | `4s` | Long enough to absorb short pauses in Codex output |
| `edit_rollover_at` | `2800` runes | Soft limit for Telegram message editing |
| Hard per-message limit | `3000` runes | Keep existing safe limit below Telegram's hard ceiling |
| Poll interval | Keep current polling model | Effective flush delay should be derived from polling interval |

## Message Lifecycle

| Step | Action |
| --- | --- |
| 1 | No message is sent if Codex answers before `working_after` |
| 2 | If Codex is still busy at `working_after`, send one `[working]` message and store its Telegram `message_id` |
| 3 | While Codex is generating, append deltas to an internal reply buffer |
| 4 | After `body_flush_after` of idle time, merge the buffered text into the active Telegram message with `editMessageText` |
| 5 | If the merged text would exceed `edit_rollover_at`, keep the current message as-is, send a new continuation message, and continue edits there |
| 6 | If a new user prompt arrives before the buffered text is flushed, flush first, then dispatch the new prompt |

## Rationale

| Choice | Why |
| --- | --- |
| Quick `working` message | Keeps response latency low for humans waiting in Telegram |
| Slower body flush | Most fragmentation comes from short internal pauses, not from true reply completion |
| Edit the active message | Avoids flooding the group with near-duplicate partial updates |
| Soft rollover before hard limit | Preserves a safety margin and keeps continuation behavior predictable |
| Flush before next dispatch | Prevents the previous answer from being lost or visually reordered |

## Non-Goals

| Not included | Reason |
| --- | --- |
| Cross-platform output differences for Lark/Feishu | This proposal is only for Telegram forwarding behavior |
| Rewriting Codex prompt format | The issue is transport behavior, not prompt content |
| Per-token live editing | Too chatty and defeats the debounce goal |

## Config Surface

| Field | Type | Default | Purpose |
| --- | --- | --- | --- |
| `working_after` | duration | `1s` | Delay before sending the first status message |
| `body_flush_after` | duration | `4s` | Idle window before flushing buffered reply text |
| `edit_rollover_at` | integer | `2800` | Soft threshold for starting a new Telegram message |

Current implementation uses internal constants for these values. YAML exposure is a follow-up.

## Acceptance Criteria

| Case | Expected result |
| --- | --- |
| Codex pauses briefly mid-reply | Telegram receives one combined body message instead of several small ones |
| Codex thinks for a while before answering | Telegram gets one early `working` status |
| A new prompt arrives right after the prior reply ends | The previous buffered reply is sent before the new prompt starts |
| Reply fits within one message | The initial `working` message is edited into the final body message |
| Reply exceeds Telegram safe size | Telegram shows a small number of continuation messages, each maintained with edit-in-place until rollover |

## Follow-Ups

1. Expose `working_after`, `body_flush_after`, and `edit_rollover_at` through YAML if runtime tuning is needed.
2. Decide whether Lark / Feishu should keep the current send-only behavior or gain a similar edit model later.
