package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
)

const (
	maxTokenQuery  = protocol.MaxTokenBytes
	maxWebhookBody = 256 << 10
)

type Server struct {
	Engine     *engine.Engine
	Store      store.Store
	Keys       hmac.Keys
	Log        *slog.Logger
	Clock      func() time.Time
	Health     func() error
	EnableMock bool
}

// Handler registers production protocol routes; mock helpers are opt-in.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /v1/start", s.start)
	mux.HandleFunc("GET /v1/portal", s.portal)
	mux.HandleFunc("GET /v1/portal/assets/portal.css", s.portalAssetCSS)
	mux.HandleFunc("GET /v1/portal/assets/portal.js", s.portalAssetJS)
	mux.HandleFunc("POST /v1/portal/cancel", s.portalCancel)
	mux.HandleFunc("GET /v1/subscriptions/{subscription_ref}", s.snapshot)
	mux.HandleFunc("POST /v1/webhooks/stripe", s.stripeWebhook)
	mux.HandleFunc("POST /v1/webhooks/adyen", s.adyenWebhook)
	if s.EnableMock {
		mux.HandleFunc("POST /v1/mock/activate", s.mockActivate)
		mux.HandleFunc("POST /v1/mock/expire", s.mockExpire)
		mux.HandleFunc("POST /v1/mock/replay", s.mockReplay)
	}
	return mux
}

// now returns the server clock.
func (s *Server) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now().UTC()
}

// health returns 200 ok when the optional health probe succeeds.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if s.Health != nil {
		if err := s.Health(); err != nil {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok"))
}

// start verifies a start token and redirects to the processor checkout URL.
func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" || len(token) > maxTokenQuery {
		writeClientError(w, http.StatusBadRequest)
		return
	}
	res, err := s.Engine.StartCheckout(r.Context(), token)
	if err != nil {
		s.mapStartError(w, err)
		return
	}
	http.Redirect(w, r, res.RedirectURL, http.StatusFound)
}

// mapStartError maps start-checkout failures to concise client status codes.
func (s *Server) mapStartError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, protocol.ErrCheckoutConflict), errors.Is(err, protocol.ErrCheckoutTerminal):
		writeClientError(w, http.StatusConflict)
	case errors.Is(err, protocol.ErrUnknownPlan):
		writeClientError(w, http.StatusBadRequest)
	case errors.Is(err, protocol.ErrExpiredToken), errors.Is(err, protocol.ErrInvalidToken),
		errors.Is(err, protocol.ErrInvalidSignature), errors.Is(err, protocol.ErrInvalidJSON),
		errors.Is(err, protocol.ErrInvalidURL), errors.Is(err, protocol.ErrIdentityField):
		writeClientError(w, http.StatusBadRequest)
	default:
		writeClientError(w, http.StatusBadRequest)
	}
}

// portal verifies a portal token and redirects to Stripe Billing or the local Adyen page.
func (s *Server) portal(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	claims, _, err := hmac.ParsePortalToken(s.Keys.Token, token, s.now())
	if err != nil {
		writeClientError(w, http.StatusBadRequest)
		return
	}
	var family, customer, shopper, checkoutID, planID, status string
	err = s.Store.InTx(r.Context(), func(tx store.Tx) error {
		sub, err := tx.GetSubscription(claims.SubscriptionRef)
		if err != nil {
			return err
		}
		family = sub.ProcessorFamily
		status = sub.Status
		planID = sub.PlanID
		checkoutID = sub.CheckoutID
		if sub.ProcessorCustomerID != nil {
			customer = *sub.ProcessorCustomerID
		}
		if sub.ProcessorShopperReference != nil {
			shopper = *sub.ProcessorShopperReference
		}
		return nil
	})
	if err != nil {
		writeClientError(w, http.StatusNotFound)
		return
	}
	if family == protocol.ProcessorStripe {
		if ad, ok := s.Engine.Adapters[protocol.ProcessorStripe]; ok && ad != nil {
			url, err := ad.CreatePortalSession(r.Context(), customer, claims.ReturnURL)
			if err != nil || url == "" {
				writeClientError(w, http.StatusBadGateway)
				return
			}
			http.Redirect(w, r, url, http.StatusFound)
			return
		}
	}
	s.renderAdyenPortal(w, r, token, claims, shopper, checkoutID, planID, status)
}

// csrfFromToken derives a bound CSRF token from the signed portal token.
func csrfFromToken(token string) string {
	sum := sha256.Sum256([]byte("csrf:" + token))
	return hex.EncodeToString(sum[:16])
}

// portalCancel records cancel-at-period-end or immediate expiry after CSRF and token checks.
func (s *Server) portalCancel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeClientError(w, http.StatusBadRequest)
		return
	}
	token := r.FormValue("token")
	if r.FormValue("csrf") != csrfFromToken(token) {
		writeClientError(w, http.StatusForbidden)
		return
	}
	claims, _, err := hmac.ParsePortalToken(s.Keys.Token, token, s.now())
	if err != nil {
		writeClientError(w, http.StatusBadRequest)
		return
	}
	if r.FormValue("mode") == "immediate" {
		if err := s.Engine.ExpireNow(r.Context(), claims.SubscriptionRef); err != nil {
			writeClientError(w, http.StatusBadRequest)
			return
		}
	} else if err := s.Engine.CancelAtPeriodEnd(r.Context(), claims.SubscriptionRef); err != nil {
		writeClientError(w, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, claims.ReturnURL, http.StatusFound)
}

// snapshot serves a signed reconciliation snapshot for a subscription_ref.
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("subscription_ref")
	path := "/v1/subscriptions/" + ref
	if err := hmac.VerifyReconcileHeader(s.Keys.Reconcile, r.Header.Get("Authorization"), path, s.now()); err != nil {
		writeClientError(w, http.StatusUnauthorized)
		return
	}
	snap, err := s.Engine.Snapshot(r.Context(), ref)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
		return
	}
	body, err := protocol.MarshalCompact(snap)
	if err != nil {
		writeClientError(w, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// stripeWebhook accepts Stripe webhook POSTs.
func (s *Server) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	s.webhook(w, r, protocol.ProcessorStripe)
}

// adyenWebhook accepts Adyen notifications and returns the required acceptance body.
func (s *Server) adyenWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, maxWebhookBody)
	if err != nil {
		writeClientError(w, http.StatusBadRequest)
		return
	}
	if err := s.Engine.IngestAndProcess(r.Context(), protocol.ProcessorAdyen, r.Header, body); err != nil {
		if errors.Is(err, protocol.ErrInvalidSignature) {
			writeClientError(w, http.StatusBadRequest)
			return
		}
		writeClientError(w, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"notificationResponse":"[accepted]"}`))
}

// webhook verifies and ingests a provider webhook for family.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request, family string) {
	body, err := readBody(r, maxWebhookBody)
	if err != nil {
		writeClientError(w, http.StatusBadRequest)
		return
	}
	if err := s.Engine.IngestAndProcess(r.Context(), family, r.Header, body); err != nil {
		if errors.Is(err, protocol.ErrInvalidSignature) {
			writeClientError(w, http.StatusBadRequest)
			return
		}
		writeClientError(w, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// mockActivate is a test helper that activates a checkout without a provider webhook.
func (s *Server) mockActivate(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("checkout_id"))
	if id == "" {
		var payload struct {
			CheckoutID string `json:"checkout_id"`
		}
		body, _ := readBody(r, 4096)
		_ = protocol.UnmarshalStrict(body, &payload, []string{"checkout_id"})
		id = payload.CheckoutID
	}
	if err := s.Engine.ActivateCheckout(r.Context(), id); err != nil {
		writeClientError(w, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mockExpire is a test helper that expires a subscription immediately.
func (s *Server) mockExpire(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.URL.Query().Get("subscription_ref"))
	if ref == "" {
		var payload struct {
			SubscriptionRef string `json:"subscription_ref"`
		}
		body, _ := readBody(r, 4096)
		_ = protocol.UnmarshalStrict(body, &payload, []string{"subscription_ref"})
		ref = payload.SubscriptionRef
	}
	if err := s.Engine.ExpireNow(r.Context(), ref); err != nil {
		writeClientError(w, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mockReplay requeues the latest stored callback for a subscription.
func (s *Server) mockReplay(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.URL.Query().Get("subscription_ref"))
	if ref == "" {
		var payload struct {
			SubscriptionRef string `json:"subscription_ref"`
		}
		body, _ := readBody(r, 4096)
		_ = protocol.UnmarshalStrict(body, &payload, []string{"subscription_ref"})
		ref = payload.SubscriptionRef
	}
	if err := s.Engine.ReplayLatest(r.Context(), ref, "mock-replay", "mock"); err != nil {
		writeClientError(w, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readBody reads a bounded request body.
func readBody(r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, max+1))
}

// writeClientError writes a concise status-text error with no internal details.
func writeClientError(w http.ResponseWriter, status int) {
	http.Error(w, http.StatusText(status), status)
}
