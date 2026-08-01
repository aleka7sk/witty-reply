# Witty Reply privacy policy

Last updated: 1 August 2026.

Witty Reply processes only content a user deliberately sends to the Telegram bot. It cannot automatically read the user's private Telegram chats.

Operator: **Alisher Tolegenov**. Support and privacy questions: [GitHub Issues](https://github.com/aleka7sk/witty-reply/issues). Do not include private conversations, access tokens, phone numbers, or other sensitive data in a public issue.

## Data processed

The service receives Telegram account identifiers, Telegram-supplied profile fields present in the update envelope (such as name, username, and language code), and the submitted text, screenshot, image document, or voice note. Submitted content is sent to the configured AI provider to choose between the `reply` and `comment` scenarios and create suggestions. The provider receives only a fixed source-type hint such as `screenshot` or `forwarded_channel`; names, usernames, and Telegram IDs are not copied into that hint. Voice notes may also be sent to the configured speech-to-text provider.

An allowlisted Belcanto operator can ask the bot to generate a first-party Threads post without submitting source content. The AI provider receives editorial controls, recent generated post text for repetition avoidance, and only the fixed fact that Belcanto is a vocal school in Astana. The operator may optionally attach one real photo; it is not sent to the AI provider. The exact text or photo-plus-text preview is sent to Meta only after the operator presses the explicit publish-confirmation button. For a photo, that button also confirms Belcanto's usage rights and any required consent from identifiable people.

## Storage

- A submitted Telegram update is written to the durable inbox only as AES-256-GCM ciphertext. The encryption key is derived separately from the runtime callback secret, never stored in PostgreSQL, and the encrypted payload is scrubbed immediately after completion or final failure. The content-free terminal job record (update/user identifiers, status, attempts, and timestamps) is then eligible for the same default seven-day hourly retention cleanup. Plaintext source content and Telegram file URLs are not written to the application database or logs.
- The Telegram numeric user ID, language code, consent time, and style settings are stored until profile deletion. Telegram username and first/last name may exist temporarily inside the encrypted update envelope but are not copied into profile or generation tables; the service does not fetch or store avatars.
- Source content and the immediately previous candidates are held in process memory while the short session is active so the user can request refinements, choose an ambiguous scenario, or correct the detected scenario without resending the source. They disappear after the configured inactivity TTL or a process restart; a valid action renews the inactivity window, while a stale button does not.
- Generated candidates, the resolved `reply` or `comment` scenario, a secret-keyed source digest, explicit feedback, and provider/model identifiers are normally retained for seven days by default, then removed by the hourly cleanup job. Token totals are exported only as content-free aggregate metrics, not stored per user or generation.
- Belcanto Threads drafts retain the generated preview, voice, editorial goal, provider/model identifiers, revision, selected format, publication state, Meta container/post identifiers, and permalink when returned. An attached photo is resized, re-encoded as JPEG to remove EXIF/GPS metadata, stored with an owner check, and normally removed by the same default seven-day cleanup. The Meta access token and Telegram file URL are never stored in a draft or media row.
- Daily usage counters are kept for quota enforcement and account operations until profile deletion; they do not contain the submitted message.
- Profile settings and style examples explicitly saved by the user are kept until reset or deletion.
- Content-free aggregated operational metrics may be retained longer.

## External processors

Telegram delivers messages to the bot. Anthropic processes AI requests when the production provider is enabled. If voice support is enabled, the voice note is first sent to the configured speech-to-text provider and the resulting transcript is then sent to the AI provider. For the operator-only Belcanto workspace, Meta receives the exact approved post and, for an image post, fetches the normalized photo from a short-lived signed HTTPS URL. The URL contains no Telegram credential or user ID. Each provider processes data under its own terms and privacy policy.

## User controls

- `/privacy` shows the in-bot summary and the deployment's public policy URL.
- `/style` shows or resets personalization.
- `/threads` creates a preview for an allowlisted Belcanto operator; optional buttons replace the text, attach/replace/remove a photo, or cancel the draft, and only the explicit publish button sends it to Meta.
- `/delete_me` permanently deletes the user's application data after confirmation.
- A user may stop processing by withdrawing consent and deleting the profile.

## Training

Private conversations and Claude outputs are not used to train a separate generative model. Any future optional contribution to a legally reusable dataset would require a separate, explicit opt-in and is not part of v2.

## Security

Production startup rejects plaintext Telegram, Anthropic, speech-provider, and Threads endpoints and PostgreSQL configurations that allow a non-TLS fallback. Secrets are provided through the runtime secret store, never committed to source control. Webhook requests and callbacks are authenticated, publishing is restricted to an exact Telegram-ID allowlist, media type and size are validated, and raw content, Threads previews, and Meta access tokens are excluded from logs.
