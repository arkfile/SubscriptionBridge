package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
)

type Notifier struct {
	Store      store.Store
	Keys       hmac.Keys
	WebhookURL string
	Client     *http.Client
	Log        *slog.Logger
	Clock      func() time.Time
	Lease      time.Duration
	RetryAt    func(attemptCount int, now time.Time) time.Time
}

// now returns the notifier clock.
func (n *Notifier) now() time.Time {
	if n.Clock != nil {
		return n.Clock()
	}
	return time.Now().UTC()
}

// client returns the HTTP client that refuses unexpected redirects.
func (n *Notifier) client() *http.Client {
	if n.Client != nil {
		return n.Client
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// RunOnce claims one due outbound event, posts the stored bytes, and records the outcome.
func (n *Notifier) RunOnce(ctx context.Context) error {
	var ev store.OutboundEvent
	err := n.Store.InTx(ctx, func(tx store.Tx) error {
		lease := n.Lease
		if lease == 0 {
			lease = 2 * time.Minute
		}
		var err error
		ev, err = tx.ClaimDueOutbound(tx.Now(), lease)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	header, err := hmac.SignCallback(n.Keys.Callback, n.now(), ev.PayloadBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.WebhookURL, bytes.NewReader(ev.PayloadBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.HMACHeaderName, header)
	resp, err := n.client().Do(req)
	if err != nil {
		return n.retry(ctx, ev, "network")
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return n.Store.InTx(ctx, func(tx store.Tx) error {
			return tx.CompleteOutbound(ev.EventID, *ev.ClaimToken, ev.FencingToken, tx.Now())
		})
	case resp.StatusCode == 408 || resp.StatusCode == 425 || resp.StatusCode == 429 || resp.StatusCode >= 500:
		return n.retry(ctx, ev, statusClass(resp.StatusCode))
	case resp.StatusCode >= 400 && resp.StatusCode <= 499:
		return n.Store.InTx(ctx, func(tx store.Tx) error {
			return tx.DeadLetterOutbound(ev.EventID, *ev.ClaimToken, ev.FencingToken, tx.Now(), statusClass(resp.StatusCode))
		})
	default:
		return n.retry(ctx, ev, statusClass(resp.StatusCode))
	}
}

// retry schedules another delivery attempt using the retry class.
func (n *Notifier) retry(ctx context.Context, ev store.OutboundEvent, class string) error {
	nextFn := n.RetryAt
	if nextFn == nil {
		nextFn = NextAttempt
	}
	next := nextFn(ev.AttemptCount, n.now())
	return n.Store.InTx(ctx, func(tx store.Tx) error {
		err := tx.RetryOutbound(ev.EventID, *ev.ClaimToken, ev.FencingToken, next, class)
		if errors.Is(err, store.ErrNotOwned) {
			return nil
		}
		return err
	})
}

// Loop repeatedly delivers due outbound callbacks until ctx is cancelled.
func (n *Notifier) Loop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := n.RunOnce(ctx); err != nil && n.Log != nil {
			n.Log.Info("notifier_cycle", "error_class", "internal")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// NextAttempt returns a jittered next-attempt time from the spec retry schedule.
func NextAttempt(attemptCount int, now time.Time) time.Time {
	bases := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
	capDelay := 6 * time.Hour
	d := capDelay
	if attemptCount >= 0 && attemptCount < len(bases) {
		d = bases[attemptCount]
	}
	if d > capDelay {
		d = capDelay
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(d)+1))
	if err != nil {
		return now.Add(d)
	}
	return now.Add(time.Duration(n.Int64()))
}

// statusClass maps an HTTP status to delivered, retry, or dead-letter.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "http_5xx"
	case code == 408:
		return "http_408"
	case code == 425:
		return "http_425"
	case code == 429:
		return "http_429"
	case code >= 400:
		return "http_4xx"
	default:
		return "http_other"
	}
}
