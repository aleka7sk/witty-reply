# Code map

This file explains behavior in product language so a maintainer can understand the system without following individual lines of Go.

## When a message arrives

The Telegram adapter validates the update, encrypts its JSON with AES-256-GCM, and commits it to the durable inbox before webhook acknowledgement or polling offset advancement. Workers atomically claim the oldest eligible job per user, retry transient failures, and scrub the encrypted payload at a terminal state. This is at-least-once processing: intake is deduplicated by `update_id`, but a reply accepted by Telegram immediately before a worker crash can be delivered again on retry. The bot then refreshes non-sensitive profile metadata, checks private-chat scope and consent, and validates the input. Screenshots and voice notes are downloaded into bounded memory; their secret Telegram URLs are never forwarded in plaintext or retained after the queue job completes.

The quota service atomically reserves the correct daily allowance before the configured AI provider is called. A per-user session marks earlier work obsolete, so a slow old generation cannot appear after the user sent a newer message. The prompt builder treats the submitted conversation as untrusted quoted data and optionally adds the user's explicitly saved style examples.

The provider must return structured output. Independent application checks enforce exactly three diverse, short replies and remove unsafe output. The service stores only the generated result, provider/model identifiers, and a secret-keyed digest of the input; token totals are exported only as content-free aggregate metrics. It sends one Telegram message with native copy buttons and signed callbacks.

## When a refinement button is pressed

The callback signature prevents forged action IDs. The generation lookup proves that the Telegram user owns the result. The short-lived session supplies the original context without requiring database storage. The new action consumes the refinement or meme allowance, creates a new version, and makes late previous work obsolete.

## When feedback is pressed

Feedback is explicit: Telegram does not reveal native copy-button clicks. A positive or negative callback is stored against the generation only after ownership is checked. Saving a candidate as “my style” is a separate explicit action and produces a reusable style example.

## When data is deleted

The user confirms deletion through a signed callback. Active in-memory context is cancelled, and PostgreSQL cascades deletion through generations, feedback, usage, entitlements, and style examples. The bot sends confirmation only after deletion succeeds.

## How AI providers differ

Anthropic Messages API is the production path. Claude CLI exists for local/internal evaluation and uses one isolated subprocess per request, structured output, no session persistence, and no tools except read-only access to a per-request temporary image when needed. The fake provider makes tests and first-run smoke checks deterministic.
