package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

type Adapter struct {
	SecretKey     string
	WebhookSecret string
	APIBase       string
	Client        *http.Client
}

// New constructs a Stripe adapter that refuses unexpected redirects.
func New(secret, webhookSecret string) *Adapter {
	return &Adapter{
		SecretKey:     secret,
		WebhookSecret: webhookSecret,
		APIBase:       "https://api.stripe.com",
		Client:        &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect},
	}
}

// noRedirect stops the HTTP client from following redirects.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Family returns stripe.
func (a *Adapter) Family() string { return protocol.ProcessorStripe }

// CreateCheckout creates a Stripe Checkout Session with a stable idempotency key.
func (a *Adapter) CreateCheckout(ctx context.Context, request adapters.CheckoutRequest) (adapters.CheckoutResult, error) {
	form := map[string]string{
		"mode":                              "subscription",
		"success_url":                       request.ReturnURL,
		"cancel_url":                        request.ReturnURL,
		"line_items[0][price]":              request.StripePriceID,
		"line_items[0][quantity]":           "1",
		"metadata[checkout_id]":             request.CheckoutID,
		"subscription_data[metadata][checkout_id]": request.CheckoutID,
	}
	if !request.ExpiresAt.IsZero() {
		form["expires_at"] = strconv.FormatInt(request.ExpiresAt.Unix(), 10)
	}
	var out struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := a.postForm(ctx, "/v1/checkout/sessions", request.IdempotencyKey, form, &out); err != nil {
		return adapters.CheckoutResult{Uncertain: adapters.IsTimeout(err)}, err
	}
	return adapters.CheckoutResult{RedirectURL: out.URL, ProcessorCheckoutID: out.ID, ExpiresAt: request.ExpiresAt}, nil
}

// CreatePortalSession creates a Stripe Billing Portal session.
func (a *Adapter) CreatePortalSession(ctx context.Context, processorCustomerID, returnURL string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	err := a.postForm(ctx, "/v1/billing_portal/sessions", "", map[string]string{
		"customer":   processorCustomerID,
		"return_url": returnURL,
	}, &out)
	return out.URL, err
}

// ParseWebhook verifies the Stripe signature and normalizes event kinds.
func (a *Adapter) ParseWebhook(_ context.Context, headers http.Header, body []byte) ([]adapters.NormalizedEvent, error) {
	if strings.TrimSpace(a.WebhookSecret) == "" {
		return nil, protocol.ErrInvalidSignature
	}
	if err := verifyStripeSignature(headers.Get("Stripe-Signature"), body, a.WebhookSecret, time.Now()); err != nil {
		return nil, err
	}
	var evt struct {
		ID   string          `json:"id"`
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &evt); err != nil {
		return nil, protocol.ErrInvalidJSON
	}
	sum := sha256.Sum256(body)
	fields := map[string]any{"authoritative_refresh_required": true, "provider_occurred_at": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)}
	kind := adapters.KindStripeSubscriptionChanged
	var data struct {
		Object map[string]any `json:"object"`
	}
	_ = json.Unmarshal(evt.Data, &data)
	obj := data.Object
	if obj == nil {
		obj = map[string]any{}
	}
	switch evt.Type {
	case "checkout.session.completed":
		kind = adapters.KindStripeCheckoutChanged
		if v, _ := obj["client_reference_id"].(string); v != "" {
			return nil, protocol.ErrIdentityField
		}
		if md, ok := obj["metadata"].(map[string]any); ok {
			if id, _ := md["checkout_id"].(string); id != "" {
				fields["checkout_id"] = id
			}
		}
		if v, _ := obj["customer"].(string); v != "" {
			fields["processor_customer_id"] = v
		}
		if v, _ := obj["subscription"].(string); v != "" {
			fields["processor_subscription_id"] = v
		}
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted", "invoice.paid", "invoice.payment_failed":
		if v, _ := obj["id"].(string); v != "" && strings.HasPrefix(v, "sub_") {
			fields["processor_subscription_id"] = v
		}
		if v, _ := obj["subscription"].(string); v != "" {
			fields["processor_subscription_id"] = v
		}
		if items, ok := nested(obj, "items", "data"); ok {
			if arr, ok := items.([]any); ok && len(arr) > 0 {
				if m, ok := arr[0].(map[string]any); ok {
					if price, ok := m["price"].(map[string]any); ok {
						if id, _ := price["id"].(string); id != "" {
							fields["provider_price_id"] = id
						}
					}
				}
			}
		}
	default:
		return nil, nil
	}
	return []adapters.NormalizedEvent{{
		ProcessorFamily:   protocol.ProcessorStripe,
		ProcessorEventID:  evt.ID,
		ProviderEventType: evt.Type,
		NormalizedKind:    kind,
		PayloadHash:       sum,
		OccurredAt:        time.Now().UTC(),
		Fields:            fields,
	}}, nil
}

// nested walks a JSON object along keys.
func nested(obj map[string]any, keys ...string) (any, bool) {
	cur := any(obj)
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[k]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// GetSubscription retrieves authoritative Stripe subscription state.
func (a *Adapter) GetSubscription(ctx context.Context, subscription adapters.ProcessorSubscription) (*adapters.SubscriptionState, error) {
	var raw map[string]any
	if err := a.get(ctx, "/v1/subscriptions/"+subscription.ProcessorSubscriptionID, &raw); err != nil {
		return nil, err
	}
	return mapStripeSub(raw)
}

// mapStripeSub maps a Stripe subscription object onto adapter state.
func mapStripeSub(raw map[string]any) (*adapters.SubscriptionState, error) {
	st := &adapters.SubscriptionState{}
	st.ProcessorSubID, _ = raw["id"].(string)
	st.ProcessorCustomerID, _ = raw["customer"].(string)
	status, _ := raw["status"].(string)
	switch status {
	case "trialing":
		st.Status = protocol.StatusTrialing
	case "active":
		st.Status = protocol.StatusActive
	case "past_due", "unpaid", "paused":
		st.Status = protocol.StatusPastDue
	case "canceled", "incomplete_expired":
		st.Status = protocol.StatusExpired
		st.Deleted = true
	case "incomplete":
		return st, nil
	default:
		st.Status = protocol.StatusPastDue
	}
	if v, ok := raw["cancel_at_period_end"].(bool); ok && v && st.Status == protocol.StatusActive {
		st.Status = protocol.StatusCanceled
		st.CancelAtPeriodEnd = true
	}
	if v, ok := asInt64(raw["current_period_start"]); ok {
		st.CurrentPeriodStart = time.Unix(v, 0).UTC().Truncate(time.Second)
	}
	if v, ok := asInt64(raw["current_period_end"]); ok {
		st.CurrentPeriodEnd = time.Unix(v, 0).UTC().Truncate(time.Second)
	}
	if items, ok := nested(raw, "items", "data"); ok {
		if arr, ok := items.([]any); ok && len(arr) > 0 {
			if m, ok := arr[0].(map[string]any); ok {
				if price, ok := m["price"].(map[string]any); ok {
					st.PlanPriceID, _ = price["id"].(string)
				}
			}
		}
	}
	return st, nil
}

// asInt64 converts JSON numbers to int64.
func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case int64:
		return t, true
	default:
		return 0, false
	}
}

// CancelSubscription cancels a Stripe subscription, optionally at period end.
func (a *Adapter) CancelSubscription(ctx context.Context, subscription adapters.ProcessorSubscription, atPeriodEnd bool) error {
	path := "/v1/subscriptions/" + subscription.ProcessorSubscriptionID
	if atPeriodEnd {
		return a.postForm(ctx, path, "", map[string]string{"cancel_at_period_end": "true"}, nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.APIBase+path, nil)
	if err != nil {
		return err
	}
	a.auth(req)
	resp, err := a.Client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("stripe cancel: %s", resp.Status)
	}
	return nil
}

// ChargeRenewal is unused for Stripe; Stripe owns the recurring schedule.
func (a *Adapter) ChargeRenewal(context.Context, adapters.RenewalRequest) (adapters.RenewalResult, error) {
	return adapters.RenewalResult{}, adapters.ErrProviderManaged
}

// ResolveRenewalAttempt is unused for Stripe; Stripe owns the recurring schedule.
func (a *Adapter) ResolveRenewalAttempt(context.Context, adapters.RenewalAttempt) (adapters.RenewalResolution, error) {
	return adapters.RenewalResolution{}, adapters.ErrProviderManaged
}

// auth sets Stripe secret-key basic auth.
func (a *Adapter) auth(req *http.Request) {
	req.SetBasicAuth(a.SecretKey, "")
}

// postForm POSTs application/x-www-form-urlencoded to Stripe.
func (a *Adapter) postForm(ctx context.Context, path, idemp string, fields map[string]string, dest any) error {
	vals := make([]string, 0, len(fields))
	for k, v := range fields {
		vals = append(vals, k+"="+v)
	}
	body := strings.NewReader(formEncode(fields))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.APIBase+path, body)
	if err != nil {
		return err
	}
	a.auth(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idemp != "" {
		req.Header.Set("Idempotency-Key", idemp)
	}
	return a.doJSON(req, dest)
}

// formEncode encodes form fields in stable key order.
func formEncode(fields map[string]string) string {
	var b strings.Builder
	first := true
	for k, v := range fields {
		if !first {
			b.WriteByte('&')
		}
		first = false
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(urlQueryEscape(v))
	}
	return b.String()
}

// urlQueryEscape percent-encodes a form value.
func urlQueryEscape(s string) string {
	replacer := strings.NewReplacer(" ", "+", "&", "%26", "=", "%3D")
	return replacer.Replace(s)
}

// get GETs a Stripe resource as JSON.
func (a *Adapter) get(ctx context.Context, path string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.APIBase+path, nil)
	if err != nil {
		return err
	}
	a.auth(req)
	return a.doJSON(req, dest)
}

// doJSON executes a Stripe request and decodes a bounded JSON body.
func (a *Adapter) doJSON(req *http.Request, dest any) error {
	resp, err := a.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("stripe http %d", resp.StatusCode)
	}
	if dest == nil {
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	return dec.Decode(dest)
}

// verifyStripeSignature checks Stripe-Signature t and v1 against the webhook secret.
func verifyStripeSignature(header string, body []byte, secret string, now time.Time) error {
	if header == "" || secret == "" {
		return protocol.ErrInvalidSignature
	}
	var ts string
	var v1 []string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "v1":
			v1 = append(v1, kv[1])
		}
	}
	if ts == "" || len(v1) == 0 {
		return protocol.ErrInvalidSignature
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return protocol.ErrInvalidSignature
	}
	if err := protocol.ValidateReplayWindow(unix, now); err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	ok := false
	for _, got := range v1 {
		if hmac.Equal([]byte(strings.ToLower(got)), []byte(expected)) {
			ok = true
			break
		}
	}
	if !ok {
		return protocol.ErrInvalidSignature
	}
	return nil
}
