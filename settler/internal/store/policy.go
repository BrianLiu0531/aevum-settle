package store

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/token"
)

// Level is an assurance level. Payments move up through the levels and never down.
type Level int16

const (
	Trusted  Level = 0 // in a block produced by the sequencer; could still be replaced
	Virtual  Level = 1 // its batch has been sequenced on L1
	Verified Level = 2 // a validity proof of its batch has been verified on L1; final
)

func (l Level) String() string {
	switch l {
	case Trusted:
		return "trusted"
	case Virtual:
		return "virtual"
	case Verified:
		return "verified"
	}
	return fmt.Sprintf("level(%d)", l)
}

// ParseLevel parses "trusted", "virtual" or "verified".
func ParseLevel(s string) (Level, error) {
	for _, l := range []Level{Trusted, Virtual, Verified} {
		if strings.EqualFold(strings.TrimSpace(s), l.String()) {
			return l, nil
		}
	}
	return 0, fmt.Errorf("unknown assurance level %q (use trusted, virtual or verified)", s)
}

// Tier applies Level to payments of at most Max (nil = any amount).
type Tier struct {
	Max   *big.Int
	Level Level
}

// Policy decides, by amount, how much assurance a payment needs before the merchant is told it
// is confirmed. Small payments can be served on the sequencer's word in about a second; large
// ones wait for the zero-knowledge proof on L1. Tiers are ordered by Max, the last is unbounded.
type Policy struct{ Tiers []Tier }

// Required returns the level a payment of amount needs.
func (p Policy) Required(amount *big.Int) Level {
	for _, t := range p.Tiers {
		if t.Max == nil || amount.Cmp(t.Max) <= 0 {
			return t.Level
		}
	}
	return Verified
}

// String renders the policy in the syntax ParsePolicy accepts.
func (p Policy) String() string {
	parts := make([]string, len(p.Tiers))
	for i, t := range p.Tiers {
		if t.Max == nil {
			parts[i] = t.Level.String()
		} else {
			parts[i] = t.Level.String() + "<=" + token.FormatAmount(t.Max)
		}
	}
	return strings.Join(parts, ",")
}

// ParsePolicy parses tiers such as "trusted<=1,virtual<=50,verified": payments up to 1 tUSD8 are
// confirmed when trusted, up to 50 when virtual, and anything larger when verified.
// A single level ("verified") applies to every amount.
func ParsePolicy(s string) (Policy, error) {
	var p Policy
	parts := strings.Split(s, ",")
	for i, part := range parts {
		name, limit, bounded := strings.Cut(strings.TrimSpace(part), "<=")
		level, err := ParseLevel(name)
		if err != nil {
			return Policy{}, err
		}
		t := Tier{Level: level}
		last := i == len(parts)-1
		switch {
		case bounded && last:
			return Policy{}, fmt.Errorf("policy %q: the last tier must have no limit", s)
		case !bounded && !last:
			return Policy{}, fmt.Errorf("policy %q: only the last tier may omit its limit", s)
		case bounded:
			if t.Max, err = token.ParseAmount(limit); err != nil {
				return Policy{}, fmt.Errorf("policy %q: %w", s, err)
			}
		}
		if i > 0 {
			prev := p.Tiers[i-1]
			if level <= prev.Level {
				return Policy{}, fmt.Errorf("policy %q: each tier must require more assurance than the one before", s)
			}
			if t.Max != nil && t.Max.Cmp(prev.Max) <= 0 {
				return Policy{}, fmt.Errorf("policy %q: tier limits must increase", s)
			}
		}
		p.Tiers = append(p.Tiers, t)
	}
	return p, nil
}
