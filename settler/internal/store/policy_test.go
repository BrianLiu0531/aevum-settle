package store

import (
	"math/big"
	"testing"
)

func TestParsePolicy(t *testing.T) {
	p, err := ParsePolicy("trusted<=1, virtual<=50.5, verified")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		raw  int64
		want Level
	}{
		{1, Trusted}, {1_000_000, Trusted}, {1_000_001, Virtual}, {50_500_000, Virtual}, {50_500_001, Verified},
	} {
		if got := p.Required(big.NewInt(c.raw)); got != c.want {
			t.Errorf("Required(%d) = %v, want %v", c.raw, got, c.want)
		}
	}
	if got := p.String(); got != "trusted<=1,virtual<=50.5,verified" {
		t.Errorf("String() = %q", got)
	}
	if one, _ := ParsePolicy("virtual"); one.Required(big.NewInt(1<<62)) != Virtual {
		t.Error("a single tier applies to every amount")
	}
}

func TestParsePolicyRejectsUnsafeOrAmbiguousTiers(t *testing.T) {
	for _, s := range []string{
		"", "final", "trusted<=1", "trusted,verified", "virtual<=1,trusted", "trusted<=5,virtual<=5,verified",
		"trusted<=1,trusted<=2,verified", "trusted<=-1,verified", "trusted<=0.0000001,verified",
	} {
		if _, err := ParsePolicy(s); err == nil {
			t.Errorf("ParsePolicy(%q) accepted", s)
		}
	}
}
