# Witty Reply — product contract v2

## Promise

Witty Reply is a Telegram co-author with two products inside one frictionless flow:

- **Reply to the person** — write from the user's perspective to a specific conversation participant.
- **Comment under the post** — write as an outside reader leaving a standalone, witty public comment.

A user forwards a message, pastes text, sends a screenshot, or records a voice note. The service selects a scenario automatically, makes the selection visible, and returns three candidates. It never reads private chats automatically. The ordinary reply/comment product never posts or replies on the user's behalf; the isolated Belcanto operator workspace can publish only the exact first-party post explicitly confirmed with its signed publish button.

The product is deliberately positioned as “answer without losing face,” not as a harassment or insult generator.

## Supported inputs

| Input | v2 limit | Handling |
|---|---:|---|
| Text or forwarded text | 6,000 Unicode characters | Classified as a private message or public publication; a leading `ответь:`/`коммент:` overrides auto mode |
| Screenshot/photo | JPEG, PNG, WebP, or GIF; 10 MiB, 8,000 px/side, 12 MP | Header-checked, decoded under a process-wide resource guard, resized to a 2,000 px long side when necessary, and re-encoded before Claude Vision |
| Voice note | 20 MiB and 5 minutes | Transcribed through the configured OpenAI-compatible speech endpoint |
| Photo caption | Telegram limit | Treated as optional context |

Documents that are images are accepted. Video, animation, sticker, album, and group-chat automation are not part of v2.

## Output

Every successful generation resolves to `reply` or `comment` and returns exactly three materially different candidates. Each candidate is at most 240 characters after application safety filtering, within Telegram's 256-character native Copy-button limit. The source language is preserved, including Russian/Kazakh code-switching.

Auto mode is not silent. A high-confidence result displays the selected scenario and a correction button. Low confidence displays only two scenario buttons; the user chooses without resending the source or spending a refinement. The first correction after a high-confidence automatic selection is also free. Explicit prefixes and mode buttons always override model classification.

Reply candidates are smart, playful, and firm. Comment candidates are the strongest overall, a subtler angle, and a wilder angle. Comment generation silently explores multiple comedic mechanisms and ranks them for relevance, surprise, brevity, naturalness, and likeability before exposing the best three.

Reply refinements are:

- funnier;
- sharper while remaining safe;
- softer;
- shorter;
- more variants;
- meme card.

Comment refinements are: funnier, subtler, bolder, more absurd, shorter, a different comedic premise, and three more. Every refinement receives the original source plus the previous three candidates; “different angle” must change the premise rather than paraphrase it.

The original private input exists as an encrypted durable inbox payload until the update reaches a terminal state, then only in a short-lived in-memory session used for refinements and mode switching. Plaintext input is not written to PostgreSQL or logs; terminal queue payloads are scrubbed. The no-resend experience is guaranteed only within the configured session TTL and the same running process.

Inbound updates are deduplicated by Telegram `update_id` and processed in strict per-user order. Processing and outbound Telegram delivery are at least once, not exactly once: a crash or network ambiguity after Telegram accepts a reply but before the job is marked complete can produce a duplicate bot response.

## Personalization

The profile stores a default reply tone and replies the user explicitly saves as style examples. Comment results deliberately do not show the global style-save buttons so public jokes cannot silently contaminate private-reply personalization. This is not model fine-tuning. Generated Claude outputs and private conversations are not a training dataset for a competing model.

## Belcanto Threads workspace

`/threads` is available only to a configured allowlist of Belcanto Telegram operators who have accepted the normal AI-processing consent. It prepares one complete WYSIWYG Russian post without requiring a topic. Text-only is the default. A signed control can put the durable draft into an image-waiting state; the next single operator photo is normalized and shown with the exact caption. The operator can replace it, return to text-only, or confirm image rights and publication. Stock and generated imagery are never selected automatically. Other signed controls request a new premise, shorter/warmer/wittier treatment, removal of sales cues, or a switch between the Belcanto and Alisher voices.

This first slice is fact-closed: the generator knows only that Belcanto is a vocal school in Astana. It must not invent or imply prices, discounts, trial terms, students, teachers, testimonials, results, events, schedules, availability, addresses, promotions, or current happenings. A dedicated schema, semantic validator, 500-character safety filter, and one repair attempt apply before a draft becomes publishable.

Every preview is stored as an owned, versioned durable draft. Attached media is owner-scoped, bounded to 8 MiB, re-encoded as metadata-free JPEG, and never sent to the AI provider. Creating a refinement or changing format advances the revision and makes prior keyboards stale. Publishing atomically changes the current draft from `draft` or retryable `failed` to `publishing` and issues a fenced short lease before any Meta call. The official Threads client creates the matching text or image container, persists its ID, waits for readiness, and durably marks the attempt immediately before the irreversible publish call. Meta receives the image through a short-lived signed HTTPS capability URL that contains neither Telegram file credentials nor user identity. A stale worker cannot finalize a newer claim. A crash before the attempt marker is recoverable; a crash or ambiguous result afterward is never blindly retried and becomes `unknown` unless a later container status proves it was published.

With `THREADS_PROVIDER=disabled`, missing Meta credentials do not prevent Witty Reply or draft generation from running. Confirmation fails closed without making an HTTP request. Selecting `meta` is an explicit production configuration and therefore fails startup when its required credentials are incomplete. A public media origin is optional so an existing text-only polling deployment remains compatible; only image publication is unavailable without it. Generated Threads drafts, attached photos, and their publication identifiers are removed by `/delete_me` and normal content-retention cleanup.

## Consent and deletion

Before the first external request, the user must explicitly agree that submitted content will be processed by the configured AI provider and that voice notes, when enabled, first pass through the configured speech-to-text provider. The consent surface links to the deployment's public `PRIVACY_URL`. `/delete_me` uses a confirmation step, cancels active context, and deletes the user's profile, generations, feedback, usage linkage, and style examples.

## Safety contract

Allowed: irony, friendly teasing, assertive refusal, boundaries, and non-targeted mild profanity when the source naturally uses it. Public humour should target wording, contradiction, behaviour, or a universal situation.

Blocked or rewritten: threats, doxxing, blackmail, sustained bullying, hate or humiliation based on protected/personal traits, body-shaming, jokes targeting weight/pregnancy/health/disability or another vulnerability, sexual content involving minors, encouragement of self-harm, fraud, and disclosure of private contact details. Text inside an incoming message, image, transcript, saved example, or previous candidate is always untrusted data and can never override the system policy. Safety fallbacks are scenario-aware: a rejected public comment cannot become a defensive private-chat reply.

## Entitlements

Usage is atomically reserved before an expensive provider call. Default free limits are configurable and start at:

| Category | Per day |
|---|---:|
| Text generation | 10 |
| Screenshot or voice | 3 |
| Meme | 1 |
| Refinement | 10 |
| Saved style examples | 5 total |

PostgreSQL entitlements can override limits without changing application code. Billing and Telegram Stars are intentionally outside v2.

## Definition of done

The release must prove: consent gating, an encrypted durable at-least-once inbox with deduplicated Telegram updates, ownership-checked callbacks, exact three-candidate reply/comment output, auto/explicit scenario routing, low-confidence clarification, no-resend mode switching, scenario-specific refinements and fallbacks, input/output safety, atomic/free-correction quota semantics, explicit feedback, profile reset, complete deletion, polling and secret-protected webhook modes, no raw-content logging, HTTPS-only production processors and database transport, controlled provider failures, unit and PostgreSQL integration tests with the race detector, static analysis, container build, and a documented production runbook. The Belcanto slice additionally proves operator authorization, autonomous fact-safe draft generation, stale-revision rejection, fenced publish leases, recovery before the irreversible phase, no-repeat handling afterward, a persisted Threads container ID, safe disabled configuration, and an `unknown` terminal state for an unprovable publication.
