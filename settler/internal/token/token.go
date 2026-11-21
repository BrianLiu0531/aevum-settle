// Package token holds network constants, the router's event signature and amount helpers.
package token

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Decimals is the number of decimal places tUSD8 uses.
const Decimals = 6

// InvoicePaidTopic is the AgentPayRouter InvoicePaid event signature.
var InvoicePaidTopic = crypto.Keccak256Hash([]byte("InvoicePaid(bytes32,address,address,address,uint256)"))

// Network describes a chain the settler can follow.
type Network struct {
	Name     string
	ChainID  uint64
	Token    common.Address
	Explorer string // base URL; tx pages are Explorer + "/tx/" + hash
	// ZkEVM means finality comes from the zkevm_* RPC namespace (virtual and verified batches).
	// Otherwise the "safe" and "finalized" block tags are used.
	ZkEVM bool
}

// Networks lists the supported chains.
var Networks = map[string]Network{
	"qday-testnet": {
		Name:     "qday-testnet",
		ChainID:  44005,
		Token:    common.HexToAddress("0x456569838f1d43009A91ebA4263147Afd7233483"),
		Explorer: "https://explorer-test.qday.info",
		ZkEVM:    true,
	},
	// A local Hardhat or anvil chain; TOKEN_ADDRESS must name the token.
	"local": {Name: "local", ChainID: 31337},
}

// Lookup returns the network with the given name.
func Lookup(name string) (Network, error) {
	n, ok := Networks[name]
	if !ok {
		return Network{}, fmt.Errorf("unknown network %q (supported: qday-testnet, local)", name)
	}
	return n, nil
}

// FormatAmount renders a raw amount as a decimal string, e.g. 1234500 -> "1.2345".
func FormatAmount(v *big.Int) string {
	if v == nil {
		return "0"
	}
	neg := v.Sign() < 0
	s := new(big.Int).Abs(v).String()
	if len(s) <= Decimals {
		s = strings.Repeat("0", Decimals-len(s)+1) + s
	}
	whole, frac := s[:len(s)-Decimals], strings.TrimRight(s[len(s)-Decimals:], "0")
	out := whole
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// ParseAmount parses a decimal string such as "2.5" into raw units.
func ParseAmount(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(frac) > Decimals || strings.HasPrefix(whole, "-") {
		return nil, fmt.Errorf("invalid amount %q", s)
	}
	v, ok := new(big.Int).SetString(whole+frac+strings.Repeat("0", Decimals-len(frac)), 10)
	if !ok {
		return nil, fmt.Errorf("invalid amount %q", s)
	}
	return v, nil
}
