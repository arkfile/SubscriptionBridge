/* generated from web/portal/portal.ts; do not edit */
(() => {
  // web/portal/portal.ts
  var CARD_STYLES = {
    base: {
      color: "#cce7ff",
      fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Oxygen, Ubuntu, Cantarell, sans-serif",
      fontSize: "16px",
      fontSmoothing: "antialiased"
    },
    error: { color: "#e05c5c" },
    placeholder: { color: "#8ab8d8" },
    validated: { color: "#00d4aa" }
  };
  function readConfig() {
    const el = document.getElementById("portal-config");
    if (!el || !el.textContent) {
      return null;
    }
    return JSON.parse(el.textContent);
  }
  function setStatus(kind, message) {
    const el = document.getElementById("dropin-status");
    if (!el) {
      return;
    }
    el.hidden = false;
    el.className = kind === "ok" ? "status-ok" : "status-err";
    el.textContent = message;
  }
  async function mountDropin() {
    const cfg = readConfig();
    if (!cfg || !cfg.available) {
      setStatus("err", "Payment method update is unavailable.");
      return;
    }
    const web = window.AdyenWeb;
    if (!web) {
      setStatus("err", "Payment method update is unavailable.");
      return;
    }
    const checkout = await web.AdyenCheckout({
      clientKey: cfg.clientKey,
      environment: cfg.environment,
      session: { id: cfg.sessionId, sessionData: cfg.sessionData },
      onPaymentCompleted: (result) => {
        if (result && result.resultCode === "Authorised") {
          setStatus("ok", "Payment method updated.");
          return;
        }
        setStatus("err", "Payment method update did not complete.");
      },
      onPaymentFailed: () => {
        setStatus("err", "Payment method update failed.");
      },
      onError: () => {
        setStatus("err", "Payment method update failed.");
      }
    });
    new web.Dropin(checkout, {
      paymentMethodsConfiguration: {
        card: { styles: CARD_STYLES, hasHolderName: false }
      }
    }).mount("#dropin");
  }
  mountDropin();
})();
