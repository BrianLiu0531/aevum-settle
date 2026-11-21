package api

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestInvoiceIDMatchesTheAgentSDK(t *testing.T) {
	// ethers.id("INV-42") in demo/sdk/agent.js
	if got, want := InvoiceID("INV-42"), crypto.Keccak256Hash([]byte("INV-42")); got != want {
		t.Fatalf("label: got %s want %s", got, want)
	}
	raw := "0x" + strings.Repeat("ab", 32)
	if got := InvoiceID(raw); got.Hex() != raw {
		t.Fatalf("raw id was rehashed: %s", got)
	}
	// 0x-prefixed but not 32 bytes: treated as a label
	if got := InvoiceID("0x1234"); got != crypto.Keccak256Hash([]byte("0x1234")) {
		t.Fatalf("short hex: %s", got)
	}
}
