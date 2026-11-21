package outbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/store"
)

func TestWebhookSignsAndReceiverVerifies(t *testing.T) {
	secret := []byte("0123456789abcdef")
	now := time.Unix(1_800_000_000, 0)
	var got struct {
		Events []map[string]any `json:"events"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := Verify(secret, r.Header.Get("X-Settle-Timestamp"), r.Header.Get("X-Settle-Signature"), body, now, 5*time.Minute); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		_ = json.Unmarshal(body, &got)
	}))
	defer srv.Close()

	p := WebhookPublisher{URL: srv.URL, Secret: secret, Explorer: "https://explorer.example", Now: func() time.Time { return now }}
	ev := store.Event{EventID: "payment.confirmed:1", Type: "payment.confirmed", Payload: json.RawMessage(`{"txHash":"0xab","amount":"1000000","logIndex":3}`)}
	if err := p.Publish(context.Background(), []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0]["eventId"] != "payment.confirmed:1" || got.Events[0]["explorerUrl"] != "https://explorer.example/tx/0xab" {
		t.Fatalf("received %+v", got.Events)
	}
	if got.Events[0]["amount"] != "1000000" {
		t.Fatalf("amount changed in transit: %v", got.Events[0]["amount"])
	}

	// A wrong secret is rejected, and so is a replay outside the window
	bad := WebhookPublisher{URL: srv.URL, Secret: []byte("wrong-secret-0000"), Now: func() time.Time { return now }}
	if err := bad.Publish(context.Background(), []store.Event{ev}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("forged webhook: %v", err)
	}
	old := WebhookPublisher{URL: srv.URL, Secret: secret, Now: func() time.Time { return now.Add(-time.Hour) }}
	if err := old.Publish(context.Background(), []store.Event{ev}); err == nil {
		t.Fatal("replayed webhook was accepted")
	}
}
