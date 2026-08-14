# Operator incident notes

## Callback not delivered

1. `bridge-cli list-events pending`
2. Inspect consumer webhook reachability; do not log payload bytes.
3. After a 2xx is confirmed, retries stop. Deterministic 4xx is `dead_lettered`.
4. `bridge-cli requeue-event <event_id> --reason "consumer fixed"` preserves event_id, version, and body.

## Uncertain Adyen charge

1. `bridge-cli scheduler-status`
2. Do not allocate a new idempotency key.
3. Resolve the existing attempt after external evidence.
4. Automatic charging stays blocked until an audited resolution.

## Unknown Stripe price

The processor event is `quarantined`. Add the price to `plans.yaml` under a new plan_id if amount/currency changed.
