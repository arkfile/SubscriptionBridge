package httpapi

import (
	"embed"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

//go:embed static/portal.css static/portal.js
var portalAssets embed.FS

const (
	adyenWebSDKVersion = "6.41.0"
	adyenWebJSSRI      = "sha384-B290qKFISOSRqrQlUy+IqPzFioaHcUn49xyVUlpEIqcSAt2+4bTkH0FOsICZ86o9"
	adyenWebCSSSRI     = "sha384-QCE3u3lliHH3SEx30M8NEuDhvEGmFCtZl62YVSVWw84cYNt2NYYbVwd/KPaaaIQ9"
)

type portalDropinConfig struct {
	Available   bool   `json:"available"`
	ClientKey   string `json:"clientKey"`
	Environment string `json:"environment"`
	SessionID   string `json:"sessionId"`
	SessionData string `json:"sessionData"`
}

// adyenCheckoutShopperBase returns the Adyen Web CDN origin for environment.
func adyenCheckoutShopperBase(env string) string {
	if env == "live" {
		return "https://checkoutshopper-live.cdn.adyen.com"
	}
	return "https://checkoutshopper-test.cdn.adyen.com"
}

// portalAssetCSS serves the hosted portal chrome stylesheet.
func (s *Server) portalAssetCSS(w http.ResponseWriter, _ *http.Request) {
	s.servePortalAsset(w, "static/portal.css", "text/css; charset=utf-8")
}

// portalAssetJS serves the compiled Drop-in init script.
func (s *Server) portalAssetJS(w http.ResponseWriter, _ *http.Request) {
	s.servePortalAsset(w, "static/portal.js", "text/javascript; charset=utf-8")
}

// servePortalAsset writes an embedded portal static file.
func (s *Server) servePortalAsset(w http.ResponseWriter, name, contentType string) {
	raw, err := portalAssets.ReadFile(name)
	if err != nil {
		writeClientError(w, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(raw)
}

// renderAdyenPortal creates a Drop-in session for the existing shopper and serves the hosted page.
func (s *Server) renderAdyenPortal(w http.ResponseWriter, r *http.Request, token string, claims protocol.PortalClaims, shopper, checkoutID, planID, status string) {
	cfg := portalDropinConfig{Environment: "test"}
	if s.Engine != nil {
		env := s.Engine.Config.AdyenEnvironment
		if env == "" {
			env = "test"
		}
		cfg.Environment = env
		cfg.ClientKey = s.Engine.Config.AdyenClientKey
	}
	if status != protocol.StatusExpired && shopper != "" && checkoutID != "" && cfg.ClientKey != "" && s.Engine != nil {
		if ad, ok := s.Engine.Adapters[protocol.ProcessorAdyen]; ok && ad != nil {
			plan, _, err := s.Engine.Config.ResolvePlan(planID)
			if err == nil {
				updateRef, err := protocol.NewPaymentUpdateReference()
				if err == nil {
					returnURL := strings.TrimRight(s.Engine.Config.PublicURL, "/") + "/v1/portal?token=" + url.QueryEscape(token)
					sess, err := ad.CreatePaymentUpdateSession(r.Context(), adapters.PaymentUpdateRequest{
						CheckoutID:      checkoutID,
						ShopperRef:      shopper,
						ReturnURL:       returnURL,
						IdempotencyKey:  updateRef,
						AmountMinor:     0,
						Currency:        plan.Currency,
						MerchantAccount: plan.Adyen.MerchantAccount,
						CountryCode:     plan.Adyen.CountryCode,
					})
					if err == nil && sess.ID != "" && sess.SessionData != "" {
						cfg.Available = true
						cfg.SessionID = sess.ID
						cfg.SessionData = sess.SessionData
					}
				}
			}
		}
	}
	s.writeAdyenPortalPage(w, token, claims.ReturnURL, cfg)
}

// writeAdyenPortalPage renders cancel forms and the Drop-in host without processor identifiers as chrome.
func (s *Server) writeAdyenPortalPage(w http.ResponseWriter, token, returnURL string, cfg portalDropinConfig) {
	csrf := csrfFromToken(token)
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		writeClientError(w, http.StatusInternalServerError)
		return
	}
	cdn := adyenCheckoutShopperBase(cfg.Environment)
	sdk := cdn + "/checkoutshopper/sdk/" + adyenWebSDKVersion
	escTok := html.EscapeString(token)
	escCSRF := html.EscapeString(csrf)
	escRet := html.EscapeString(returnURL)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	page := `<!DOCTYPE html><html lang="en"><head>` +
		`<meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1">` +
		`<title>Manage subscription</title>` +
		`<link rel="stylesheet" href="` + sdk + `/adyen.css" integrity="` + adyenWebCSSSRI + `" crossorigin="anonymous">` +
		`<link rel="stylesheet" href="/v1/portal/assets/portal.css">` +
		`</head><body><main class="portal-card">` +
		`<h1>Manage subscription</h1>` +
		`<section class="billing-panel-section">` +
		`<h4>Update payment method</h4>` +
		`<div id="dropin"></div>` +
		`<p id="dropin-status" hidden></p>` +
		`</section>` +
		`<section class="billing-panel-section">` +
		`<h4>Cancel</h4>` +
		`<div class="portal-actions">` +
		`<form method="post" action="/v1/portal/cancel">` +
		`<input type="hidden" name="token" value="` + escTok + `">` +
		`<input type="hidden" name="csrf" value="` + escCSRF + `">` +
		`<input type="hidden" name="return_url" value="` + escRet + `">` +
		`<input type="hidden" name="mode" value="period_end">` +
		`<button type="submit" class="btn-secondary">Cancel at period end</button></form>` +
		`<form method="post" action="/v1/portal/cancel">` +
		`<input type="hidden" name="token" value="` + escTok + `">` +
		`<input type="hidden" name="csrf" value="` + escCSRF + `">` +
		`<input type="hidden" name="return_url" value="` + escRet + `">` +
		`<input type="hidden" name="mode" value="immediate">` +
		`<button type="submit" class="danger-button">Cancel immediately</button></form>` +
		`</div></section>` +
		`<a class="return-link" href="` + escRet + `">Return to application</a>` +
		`</main>` +
		`<script type="application/json" id="portal-config">` + string(cfgJSON) + `</script>` +
		`<script src="` + sdk + `/adyen.js" integrity="` + adyenWebJSSRI + `" crossorigin="anonymous"></script>` +
		`<script src="/v1/portal/assets/portal.js"></script>` +
		`</body></html>`
	_, _ = w.Write([]byte(page))
}
