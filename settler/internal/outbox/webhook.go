package outbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/store"
)

// WebhookPublisher POSTs each batch of events to the merchant as {"events": [...]}.
//
// The body is signed so the receiver can reject forgeries and replays:
//
//	X-Settle-Timestamp: unix seconds
//	X-Settle-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))
//
// Any non-2xx answer fails the batch and the relay retries it, so delivery is at least once and
// the receiver must be idempotent on eventId. See Verify for the receiving side.
type WebhookPublisher struct {
	URL      string
	Secret   []byte
	Explorer string // when set, each event gets an explorerUrl for its transaction
	Client   *http.Client
	Now      func() time.Time
}

// Sign returns the signature header value for a body sent at the given time.
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a received webhook: the signature must match and the timestamp must be within
// maxSkew of now, so a captured request cannot be replayed later.
func Verify(secret []byte, timestamp, signature string, body []byte, now time.Time, maxSkew time.Duration) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("bad timestamp")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > maxSkew || d < -maxSkew {
		return fmt.Errorf("timestamp outside the allowed window")
	}
	if !hmac.Equal([]byte(signature), []byte(Sign(secret, timestamp, body))) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// Message is the JSON body of one event: its payload plus eventId (and explorerUrl when known).
func Message(e store.Event, explorer string) (json.RawMessage, error) {
	var payload map[string]any
	dec := json.NewDecoder(bytes.NewReader(e.Payload))
	dec.UseNumber() // keep numbers exact (no float64 formatting such as 1e+06)
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("event %s: %w", e.EventID, err)
	}
	payload["eventId"] = e.EventID
	if tx, ok := payload["txHash"].(string); ok && explorer != "" {
		payload["explorerUrl"] = explorer + "/tx/" + tx
	}
	return json.Marshal(payload)
}

func (p WebhookPublisher) Publish(ctx context.Context, events []store.Event) error {
	messages := make([]json.RawMessage, len(events))
	for i, e := range events {
		m, err := Message(e, p.Explorer)
		if err != nil {
			return err
		}
		messages[i] = m
	}
	body, err := json.Marshal(map[string]any{"events": messages})
	if err != nil {
		return err
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	ts := strconv.FormatInt(now().Unix(), 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Settle-Timestamp", ts)
	req.Header.Set("X-Settle-Signature", Sign(p.Secret, ts, body))

	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return fmt.Errorf("webhook answered %d: %s", res.StatusCode, bytes.TrimSpace(snippet))
	}
	return nil
}
