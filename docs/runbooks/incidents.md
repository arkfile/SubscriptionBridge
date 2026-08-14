# Operator incident notes

Do not log pairing roots, derived keys, payment-method references, shopper references, provider credentials, or raw provider payloads.

## Callback not delivered

1. `bridge-cli list-events pending`
2. Inspect consumer webhook reachability; do not log payload bytes.
3. After a 2xx is confirmed, retries stop. Deterministic 4xx is `dead_lettered`.
4. `bridge-cli requeue-event <event_id> --reason "consumer fixed"` preserves event_id, version, and body.
5. `bridge-cli abandon-event <event_id> --reason "..."` is terminal. Requeue later if the consumer is repaired.

## Uncertain Adyen charge

1. `bridge-cli scheduler-status`
2. Do not allocate a new idempotency key or issue another `/payments` call with a new key.
3. Confirm the attempt in Adyen Customer Area using the existing merchant reference / idempotency key.
4. `bridge-cli resolve-attempt <attempt_id> --outcome authorized|refused|expired --reason "..."` after external evidence.
5. Automatic charging stays blocked until an audited authorized resolution or payment-method replacement.

## Unknown Stripe price

The processor event is `quarantined`. Add a new `plan_id` in `plans.yaml` if amount or currency changed. Do not map the unknown price onto an existing plan.

## Payment-method replacement (Adyen)

The hosted `/v1/portal` Drop-in session reuses the existing shopper. A successful `AUTHORISATION` webhook re-encrypts the stored payment method and clears `automatic_charging_blocked` without consuming `state_version`. If the subscription is `past_due`, the next scheduled renewal may retry automatically after the block clears.

## Immediate cancel

Immediate cancel on the Adyen portal expires the subscription now. Period-end cancel remains effective through `current_period_end`. Paid cancels are portal or processor dashboard actions, not the consumer admin CLI.

## Backup and restore

1. Stop the bridge process (or keep it read-only) before a logical restore.
2. Backup with PostgreSQL tools only, for example `pg_dump --format=custom` of the bridge database. Do not copy data-encryption keys into the dump notes or tickets.
3. Restore with `pg_restore` onto an empty database, then `bridge-cli migrate` if the dump predates the current schema.
4. Confirm `bridge-cli health` and `bridge-cli scheduler-status` before enabling the scheduler.
5. Pairing root, `ADYEN_DATA_ENCRYPTION_KEY`, and provider secrets are not in the database; restore them from the secret manager separately. A dump without the matching encryption key cannot decrypt stored payment methods or replay bodies.

## After restore

1. `bridge-cli list-events pending` and requeue only if the consumer did not already apply those versions.
2. Inspect uncertain or `manual_review` attempts before the scheduler runs.
3. Do not replay Adyen charges with a new idempotency key.
