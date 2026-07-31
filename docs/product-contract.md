# Witty Reply — product contract v2

## Promise

Witty Reply is a Telegram co-author with two products inside one frictionless flow:

- **Reply to the person** — write from the user's perspective to a specific conversation participant.
- **Comment under the post** — write as an outside reader leaving a standalone, witty public comment.

A user forwards a message, pastes text, sends a screenshot, or records a voice note. The service selects a scenario automatically, makes the selection visible, and returns three candidates. It never reads private chats automatically and never posts or replies on the user's behalf.

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

The release must prove: consent gating, an encrypted durable at-least-once inbox with deduplicated Telegram updates, ownership-checked callbacks, exact three-candidate output, auto/explicit scenario routing, low-confidence clarification, no-resend mode switching, scenario-specific refinements and fallbacks, input/output safety, atomic/free-correction quota semantics, explicit feedback, profile reset, complete deletion, polling and secret-protected webhook modes, no raw-content logging, HTTPS-only production processors and database transport, controlled provider failures, unit and PostgreSQL integration tests with the race detector, static analysis, container build, and a documented production runbook.
