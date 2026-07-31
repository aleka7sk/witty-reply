# Operations runbook

## Initial Telegram setup

1. Create the bot with `@BotFather` and copy the token into the deployment secret store.
2. Disable group joining with `/setjoingroups` unless group support is deliberately added later.
3. Publish `docs/privacy-policy.md` at a stable HTTPS URL, set the same value as `PRIVACY_URL`, and register it with `/setprivacypolicy`.
4. Set the command menu from the command list in the README.
5. Use polling for a single local instance. Use an HTTPS webhook for production.

Never place the bot token in a webhook URL, log, issue, screenshot, or committed `.env` file.

## Local smoke test

1. Copy `.env.example` to `.env` and set `TELEGRAM_BOT_TOKEN` plus a random callback secret.
2. Keep `AI_PROVIDER=fake` to validate the whole Telegram flow without API spend.
3. Run `docker compose up --build`.
4. Wait until both services are healthy with `docker compose ps`, then check `curl http://localhost:8080/readyz`.
5. Send `/start`, accept processing, and exercise both scenarios:
   - send a direct message to see `↩️ Ответить человеку`;
   - send `коммент: <публичный пост>` to see `🔥 Залететь в комменты`;
   - use the scenario-switch button and confirm that the original source is reused.
6. In fake mode the scenarios, refinements, and revisions intentionally produce different deterministic demo text.
7. Set `BELCANTO_OPERATOR_IDS` to your numeric Telegram ID and `THREADS_PROVIDER=fake`. Send `/threads`, inspect the exact preview, press publish twice, and verify that only the first confirmation succeeds.
8. Switch to `AI_PROVIDER=anthropic`, add an Anthropic API key, and restart for a real-provider test.

## Connect Threads

1. Create a Meta app with the Threads API use case, add the Belcanto account as a tester during development, and accept that invitation in Threads.
2. Authorize `threads_basic` and `threads_content_publish`; exchange the returned one-hour token for a long-lived token and arrange renewal before expiry.
3. Store `THREADS_USER_ID` and `THREADS_ACCESS_TOKEN` only in the deployment secret store. Set `THREADS_PROVIDER=meta`, the operator allowlist, and `THREADS_API_BASE_URL=https://graph.threads.net/v1.0`.
4. Restart, send `/threads`, and publish one deliberate test post. A successful confirmation must leave the draft in `published`; a double tap must not create a second Meta request.
5. If the bot reports an unknown outcome, press the same publish button once to request a status-only reconciliation. The bot may mark it published only when Meta reports `PUBLISHED`; it will not issue another publish request.
6. If the result remains unknown, inspect the Belcanto account manually. Do not reset the row or create another identical post until the external result is reconciled.

## Production checklist

- `APP_ENV=production`.
- `PRIVACY_URL` is the published HTTPS policy operated by Alisher Tolegenov; support points to [GitHub Issues](https://github.com/aleka7sk/witty-reply/issues).
- `TELEGRAM_API_BASE_URL=https://api.telegram.org`; other hosts and plaintext endpoints are rejected.
- `STORE_DRIVER=postgres` with managed backups and a `DATABASE_URL` that explicitly uses `sslmode=require`, `verify-ca`, or `verify-full`.
- `AI_PROVIDER=anthropic`; never public OAuth/Max credentials.
- `ANTHROPIC_BASE_URL` and any enabled `SPEECH_BASE_URL` use HTTPS without embedded credentials.
- Secret-protected webhook on TLS 1.2+.
- Unique 32+ byte callback and webhook secrets.
- Voice provider either fully configured or deliberately disabled.
- Threads is either deliberately disabled or configured with the Belcanto user ID, a long-lived token, an exact operator Telegram-ID allowlist, and the official HTTPS Graph host.
- `/healthz`, `/readyz`, and `/metrics` monitored.
- `/metrics` is restricted to the monitoring network or protected at the ingress.
- Logs have a 30-day-or-shorter retention and contain no user content.
- PostgreSQL disk encryption and least-privilege network access enabled.
- Daily database backups tested for restore.
- Backup retention and expiry are documented so a profile deleted from the live database is not retained indefinitely in backups.
- Exactly one application replica is used for the process-local scenario session. Sticky HTTP routing is not sufficient because workers claim jobs from the shared durable inbox. The original source expires after `SESSION_TTL` of inactivity and is lost on restart; durable shared source storage and consistent per-user worker ownership are not part of v2.

## Health and alerts

- `/healthz` proves that the HTTP process can respond; it does not probe workers or external dependencies.
- `/readyz` checks store connectivity and the required PostgreSQL schema, and returns non-200 when the bot should not receive webhook traffic. It does not probe Telegram or AI-provider availability.
- `/metrics` exposes Prometheus-format, content-free counters, gauges, and latency histograms.
- Scenario metrics use only fixed `reply`/`comment` names. They never include submitted text, captions, usernames, or free-form source hints.

Alert when the five-minute error rate exceeds 5%, provider p95 latency exceeds 30 seconds, provider rate limits spike, readiness fails, retry/dead-letter counters rise, the oldest pending inbox job exceeds five minutes, or the cleanup worker has not completed for two hours.

The queue is deliberately at least once. During incident review, treat a duplicate bot response as a possible outbound acknowledgement ambiguity: Telegram may have accepted the send before a worker crash, while the durable job remained retryable.

## Secret rotation

1. Rotate the exposed provider key or Telegram token at its issuer.
2. Update the runtime secret store.
3. Restart or roll the application.
4. If the bot token changed, re-register the webhook.
5. Before rotating the callback secret, pause intake and let the encrypted inbox drain. Rotation invalidates existing inline buttons and makes any still-pending inbox ciphertext intentionally unreadable.
6. Rotate or refresh the Threads long-lived token independently; never place it in logs or a support message. Re-run Meta OAuth if its permission grant has expired.

## Rollback

Application migrations are additive and idempotent in v2. Roll back to the previous container image without reversing the schema. Validate readiness, `/start`, and one fake-provider generation before restoring normal traffic.
