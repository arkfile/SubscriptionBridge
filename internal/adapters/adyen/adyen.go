package adyen

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

const APIVersion = "v71"

type Adapter struct {
	APIKey      string
	HMACKey     string
	Environment string
	LivePrefix  string
	Client      *http.Client
}

// New constructs an Adyen Checkout API adapter.
func New(apiKey, hmacKey, env, livePrefix string) *Adapter {
	return &Adapter{
		APIKey:      apiKey,
		HMACKey:     hmacKey,
		Environment: env,
		LivePrefix:  livePrefix,
		Client:      &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// Family returns adyen.
func (a *Adapter) Family() string { return protocol.ProcessorAdyen }

// CheckoutEndpoint returns the test or live Checkout API base URL.
func (a *Adapter) CheckoutEndpoint() string {
	if a.Environment == "live" {
		return fmt.Sprintf("https://%s-checkout-live.adyenpayments.com/checkout/%s", a.LivePrefix, APIVersion)
	}
	return "https://checkout-test.adyen.com/checkout/" + APIVersion
}

// PaymentsURL returns the payments endpoint.
func (a *Adapter) PaymentsURL() string { return a.CheckoutEndpoint() + "/payments" }

// CreateCheckout creates an Adyen hosted checkout session with a bridge shopper reference.
func (a *Adapter) CreateCheckout(ctx context.Context, request adapters.CheckoutRequest) (adapters.CheckoutResult, error) {
	payload := map[string]any{
		"merchantAccount":         request.MerchantAccount,
		"amount":                  map[string]any{"value": request.AmountMinor, "currency": request.Currency},
		"reference":               request.CheckoutID,
		"returnUrl":               request.ReturnURL,
		"countryCode":             request.CountryCode,
		"shopperReference":        request.ShopperRef,
		"storePaymentMethod":      true,
		"shopperInteraction":      "Ecommerce",
		"recurringProcessingModel": "Subscription",
		"metadata":                map[string]string{"checkout_id": request.CheckoutID},
	}
	var out struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := a.postJSON(ctx, a.CheckoutEndpoint()+"/sessions", request.IdempotencyKey, payload, &out); err != nil {
		return adapters.CheckoutResult{Uncertain: adapters.IsTimeout(err)}, err
	}
	redirect := out.URL
	if redirect == "" {
		redirect = request.ReturnURL
	}
	return adapters.CheckoutResult{RedirectURL: redirect, ProcessorCheckoutID: out.ID, ExpiresAt: request.ExpiresAt}, nil
}

// QA / TODO: Adyen portal is locally hosted; this echoes returnURL and is not a provider portal.
func (a *Adapter) CreatePortalSession(_ context.Context, _, returnURL string) (string, error) {
	return returnURL, nil
}

// ParseWebhook verifies each notification item HMAC and normalizes event kinds.
func (a *Adapter) ParseWebhook(_ context.Context, _ http.Header, body []byte) ([]adapters.NormalizedEvent, error) {
	var note struct {
		NotificationItems []struct {
			NotificationRequestItem map[string]any `json:"NotificationRequestItem"`
		} `json:"notificationItems"`
	}
	if err := json.Unmarshal(body, &note); err != nil {
		return nil, protocol.ErrInvalidJSON
	}
	var out []adapters.NormalizedEvent
	for _, wrap := range note.NotificationItems {
		item := wrap.NotificationRequestItem
		if item == nil {
			continue
		}
		if err := verifyAdyenItem(item, a.HMACKey); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		eventCode, _ := item["eventCode"].(string)
		success := stringify(item["success"]) == "true"
		psp, _ := item["pspReference"].(string)
		orig, _ := item["originalReference"].(string)
		merchantRef, _ := item["merchantReference"].(string)
		id := strings.Join([]string{psp, eventCode, stringify(item["success"]), orig}, ":")
		fields := map[string]any{
			"processor_payment_id": psp,
			"provider_status":      eventCode,
			"provider_occurred_at": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
			"success":              success,
		}
		kind := adapters.KindAdyenOperationalAdjustment
		switch eventCode {
		case "AUTHORISATION":
			if strings.HasPrefix(merchantRef, protocol.AttemptPrefix) {
				kind = adapters.KindAdyenRenewalAuthorisation
				fields["attempt_reference"] = merchantRef
				if !success {
					if rc, _ := item["reason"].(string); rc != "" {
						fields["refusal_code"] = rc
					}
				}
			} else {
				kind = adapters.KindAdyenInitialAuthorisation
				if strings.HasPrefix(merchantRef, protocol.CheckoutPrefix) {
					fields["checkout_id"] = merchantRef
				}
			}
		case "CANCELLATION", "CANCEL_OR_REFUND":
			kind = adapters.KindAdyenContractChanged
			if orig != "" {
				fields["processor_payment_id"] = orig
			}
		}
		n := adapters.NormalizedEvent{
			ProcessorFamily:   protocol.ProcessorAdyen,
			ProcessorEventID:  id,
			ProviderEventType: eventCode,
			NormalizedKind:    kind,
			PayloadHash:       sum,
			OccurredAt:        time.Now().UTC(),
			Fields:            fields,
		}
		if success && eventCode == "AUTHORISATION" && kind == adapters.KindAdyenInitialAuthorisation {
			if add, ok := item["additionalData"].(map[string]any); ok {
				if tok := stringify(add["recurring.recurringDetailReference"]); tok != "" {
					n.SensitivePlain = []byte(tok)
				} else if tok := stringify(add["tokenization.storedPaymentMethodId"]); tok != "" {
					n.SensitivePlain = []byte(tok)
				}
			}
		}
		out = append(out, n)
	}
	return out, nil
}

// stringify renders Adyen JSON scalars for HMAC payload construction.
func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatInt(int64(t), 10)
	default:
		return fmt.Sprint(v)
	}
}

// verifyAdyenItem checks the Adyen notification HMAC over the canonical field list.
func verifyAdyenItem(item map[string]any, hmacKeyHex string) error {
	if strings.TrimSpace(hmacKeyHex) == "" {
		return protocol.ErrInvalidSignature
	}
	add, _ := item["additionalData"].(map[string]any)
	if add == nil {
		return protocol.ErrInvalidSignature
	}
	got := stringify(add["hmacSignature"])
	if got == "" {
		return protocol.ErrInvalidSignature
	}
	key, err := hex.DecodeString(hmacKeyHex)
	if err != nil {
		return protocol.ErrInvalidSignature
	}
	amount, _ := item["amount"].(map[string]any)
	payload := strings.Join([]string{
		stringify(item["pspReference"]),
		stringify(item["originalReference"]),
		stringify(item["merchantAccountCode"]),
		stringify(item["merchantReference"]),
		stringify(amount["value"]),
		stringify(amount["currency"]),
		stringify(item["eventCode"]),
		stringify(item["success"]),
	}, ":")
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if len(got) != len(want) || !hmac.Equal([]byte(got), []byte(want)) {
		return protocol.ErrInvalidSignature
	}
	return nil
}

// QA / TODO: stub; Adyen has no provider-owned subscription object to retrieve.
func (a *Adapter) GetSubscription(context.Context, adapters.ProcessorSubscription) (*adapters.SubscriptionState, error) {
	return nil, nil
}

// QA / TODO: stub; cancel is recorded locally and does not call Adyen.
func (a *Adapter) CancelSubscription(context.Context, adapters.ProcessorSubscription, bool) error {
	return nil
}

// CanonicalPaymentBody builds the exact JSON bytes used for recurring ContAuth charges.
func CanonicalPaymentBody(merchant, currency, reference, shopper, token string, amount int64) []byte {
	var b bytes.Buffer
	b.WriteString(`{"merchantAccount":`)
	writeJSONString(&b, merchant)
	b.WriteString(`,"amount":{"value":`)
	b.WriteString(strconv.FormatInt(amount, 10))
	b.WriteString(`,"currency":`)
	writeJSONString(&b, strings.ToUpper(currency))
	b.WriteString(`},"reference":`)
	writeJSONString(&b, reference)
	b.WriteString(`,"shopperReference":`)
	writeJSONString(&b, shopper)
	b.WriteString(`,"shopperInteraction":"ContAuth","recurringProcessingModel":"Subscription","storedPaymentMethodId":`)
	writeJSONString(&b, token)
	b.WriteByte('}')
	return b.Bytes()
}

// writeJSONString appends a JSON-encoded string to the canonical body.
func writeJSONString(b *bytes.Buffer, s string) {
	enc, _ := json.Marshal(s)
	b.Write(enc)
}

// ChargeRenewal POSTs the persisted canonical body with the stable idempotency key.
func (a *Adapter) ChargeRenewal(ctx context.Context, request adapters.RenewalRequest) (adapters.RenewalResult, error) {
	return a.sendExact(ctx, request.Endpoint, request.IdempotencyKey, request.CanonicalBody)
}

// ResolveRenewalAttempt replays the exact stored request bytes with the same idempotency key.
func (a *Adapter) ResolveRenewalAttempt(ctx context.Context, attempt adapters.RenewalAttempt) (adapters.RenewalResolution, error) {
	res, err := a.sendExact(ctx, attempt.Endpoint, attempt.IdempotencyKey, attempt.CanonicalBody)
	return adapters.RenewalResolution{
		Status:             res.Status,
		ProcessorPaymentID: res.ProcessorPaymentID,
		RefusalCode:        res.RefusalCode,
		Uncertain:          res.Uncertain,
	}, err
}

// sendExact POSTs exact request bytes and classifies authorised, refused, or uncertain.
func (a *Adapter) sendExact(ctx context.Context, endpoint, idemp string, body []byte) (adapters.RenewalResult, error) {
	if endpoint == "" {
		endpoint = a.PaymentsURL()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return adapters.RenewalResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", a.APIKey)
	req.Header.Set("Idempotency-Key", idemp)
	resp, err := a.Client.Do(req)
	if err != nil {
		return adapters.RenewalResult{Uncertain: true}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 500 || resp.StatusCode == 0 {
		return adapters.RenewalResult{Uncertain: true}, fmt.Errorf("adyen http %d", resp.StatusCode)
	}
	var parsed struct {
		PspReference string `json:"pspReference"`
		ResultCode   string `json:"resultCode"`
		RefusalReasonCode string `json:"refusalReasonCode"`
	}
	_ = json.Unmarshal(raw, &parsed)
	switch parsed.ResultCode {
	case "Authorised":
		return adapters.RenewalResult{Status: "authorized", ProcessorPaymentID: parsed.PspReference}, nil
	case "Refused", "Cancelled", "Error":
		return adapters.RenewalResult{Status: "refused", ProcessorPaymentID: parsed.PspReference, RefusalCode: parsed.RefusalReasonCode}, nil
	default:
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 409 {
			return adapters.RenewalResult{Status: "refused", RefusalCode: strconv.Itoa(resp.StatusCode)}, nil
		}
		return adapters.RenewalResult{Uncertain: true}, nil
	}
}

// postJSON POSTs a JSON payload to the Adyen Checkout API.
func (a *Adapter) postJSON(ctx context.Context, url, idemp string, payload any, dest any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", a.APIKey)
	if idemp != "" {
		req.Header.Set("Idempotency-Key", idemp)
	}
	resp, err := a.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("adyen http %d", resp.StatusCode)
	}
	if dest == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(dest)
}

// AcceptanceResponse is the required Adyen notification acknowledgement body.
func AcceptanceResponse() []byte {
	return []byte(`{"notificationResponse":"[accepted]"}`)
}
