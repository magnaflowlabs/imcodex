# Telegram Output Buffering

## Status

Current behavior for `v2.2.2`. This document describes what ships today, not a
future proposal.

## Goals

- avoid Telegram `429` amplification caused by our own retry behavior
- avoid replaying already delivered body text
- keep long-running replies visible without turning every small delta into a new
  message

## Current Behavior

### Editable Body Path

- Telegram replies still prefer editable messages when the messenger supports
  them.
- Body updates respect the normal `editableSyncEvery` cadence, including the
  busy-to-idle transition.
- A short `[working]` status message may appear first and is cleaned up
  independently from body delivery.

### Detached Queue Path

- Plain detached chunks are queued in order.
- After backoff expires, the queue resumes one chunk at a time.
- A minimum per-chat spacing is applied even when Telegram is not currently
  rate-limiting.
- Non-`429` transport failures keep the queue head and retry with bounded
  backoff.

### Shared Transport Safety

- send, edit, delete, and chat-action calls all use bounded request timeouts
- Telegram `retry_after` is honored
- a detached `429` blocks editable sends during the same backoff window
- an editable `429` blocks detached sends during the same backoff window

## Behaviors Removed In `v2.2.2`

- detached backlog drain loops that send many chunks immediately after a retry
  window
- watchdog-triggered mutation from editable body delivery to plain detached body
  delivery
- forced editable flushes that bypass the nominal sync interval every time a run
  becomes idle
- body transport calls made with `context.Background()`

## Known Limits

- delivery state is still kept in memory; there is no persisted send journal yet
- a timed-out request can still be ambiguous if Telegram received it but the
  client did not receive the response
- tmux snapshot tracking and delivery tracking are still more coupled than they
  should be

## Related Docs

- [message-delivery-redesign.md](message-delivery-redesign.md): next-step
  simplification plan
- [runtime-v2-docker-tmux.md](runtime-v2-docker-tmux.md): runtime model
