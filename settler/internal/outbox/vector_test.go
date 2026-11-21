package outbox

import "testing"

// TestSignVector pins the signature format; demo/test/sdk.test.js checks the same vector, so the
// Go sender and the JavaScript receiver cannot drift apart.
func TestSignVector(t *testing.T) {
	const want = "sha256=044db41e5630c9e1eda7fa2c624a922420b9a745fc2c2eb0ed58b256224bd484"
	if got := Sign([]byte("0123456789abcdef"), "1800000000", []byte(`{"events":[]}`)); got != want {
		t.Fatalf("got %s", got)
	}
}
