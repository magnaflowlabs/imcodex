# Message Delivery Redesign

## Status

Phase 1 shipped in `v2.2.2`.

This document now tracks the remaining cleanup work after the first transport
stabilization pass.

## What Phase 1 Already Changed

Implemented in the current codebase:

- bounded request timeouts for outbound send/edit/delete/chat-action calls
- paced detached retries instead of whole-backlog drain loops
- shared backoff between editable and detached delivery paths
- no watchdog-triggered rewrite from editable body delivery to plain detached
  body delivery
- no forced busy-to-idle editable flush that bypasses the normal sync interval

These changes target the worst production amplifiers first:

- burst sends after `retry_after`
- duplicated late replay of old body text
- recovery that only resumes after a later unrelated inbound message

## What Is Still Wrong

The implementation is safer than before, but the design is still heavier than it
should be.

Remaining structural issues:

- tmux observation state and delivery state still live in the same runtime object
- body publishing still supports both editable and detached paths
- delivery correctness still depends on snapshot reconciliation rules that are
  harder to reason about than a true sender queue
- delivery state is still in memory only

## Target End State

The longer-term target remains:

1. a run tracker that only interprets tmux state
2. a chat-scoped sender that owns retry timing and delivery ordering
3. a thin transport adapter that only performs API calls and parses errors

## Design Constraints

The next phases should preserve these rules:

- never resend already acknowledged body chunks
- never let watchdogs mutate delivery history
- keep at most one outbound request in flight per chat
- keep status-message UX separate from body-delivery correctness
- prefer paced recovery over visual polish

## Next Phases

### Phase 2: Collapse Delivery Modes

Goals:

- reduce body delivery to one conservative strategy per run
- make sender state explicit instead of implicit in poll-loop fields
- move retry clocks out of snapshot reconciliation logic

### Phase 3: Split Tracker From Sender

Goals:

- emit logical run events from tmux tracking
- let the sender consume queued outbound operations independently
- make delivery progress depend on sender timers, not on future unrelated polls

## Acceptance Criteria

The redesign is complete when all of these are true:

- a Telegram `429` no longer causes later burst replay of already delivered body
  text
- body delivery progress is explained by sender state, not by watchdog side
  effects
- one chat has one serialized outbound pipeline
- retries continue on time without waiting for a new inbound message
- the mutable per-run delivery state is materially smaller than today
