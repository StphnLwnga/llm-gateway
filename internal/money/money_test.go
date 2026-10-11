package money

import "testing"

func TestParseUSDRoundTripsExactly(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"0":           "0.000000",
		"0.15":        "0.150000",
		"0.000150":    "0.000150",
		"0.000001":    "0.000001",
		"2.5":         "2.500000",
		"15":          "15.000000",
		".5":          "0.500000",
		"99999999.99": "99999999.990000",
	}
	for in, want := range cases {
		m, err := ParseUSD(in)
		if err != nil {
			t.Errorf("ParseUSD(%q): %v", in, err)
			continue
		}
		if got := m.String(); got != want {
			t.Errorf("ParseUSD(%q).String() = %q, want %q", in, got, want)
		}
		if back, err := ParseUSD(m.String()); err != nil || back != m {
			t.Errorf("round trip of %q: %v, %v", in, back, err)
		}
	}
	if m, _ := ParseUSD("0.000150"); m != 150 {
		t.Errorf("0.000150 USD = %d micro-USD, want 150", m)
	}
}

func TestParseUSDRejectsBadAmounts(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "-0.15", "+1", "0.0000001", "1.", "abc", "1e3", "1,5", "0.1.2", "9999999999999999999"} {
		if _, err := ParseUSD(in); err == nil {
			t.Errorf("ParseUSD(%q) succeeded, want an error", in)
		}
	}
}

func TestStringOfANegativeAmount(t *testing.T) {
	t.Parallel()
	if got := MicroUSD(-1_500_000).String(); got != "-1.500000" {
		t.Errorf("String() = %q, want -1.500000", got)
	}
	if got := MicroUSD(-1 << 63).String(); got != "-9223372036854.775808" {
		t.Errorf("String() of the minimum = %q", got)
	}
}
