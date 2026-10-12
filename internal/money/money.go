// Package money holds exact amounts of US dollars.
package money

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

// MicroUSD is an amount in millionths of a US dollar. Integer arithmetic keeps
// every price and cost exact; a float would not.
type MicroUSD int64

// ParseUSD reads a non-negative dollar amount with at most six decimal places,
// for example "0.15" or "0.000150".
func ParseUSD(s string) (MicroUSD, error) {
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" && !hasFrac || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return 0, fmt.Errorf("amount %q: must be a non-negative number of dollars, for example 0.15", s)
	}
	if len(frac) > 6 {
		return 0, fmt.Errorf("amount %q: at most 6 decimal places", s)
	}
	if hasFrac && frac == "" {
		return 0, fmt.Errorf("amount %q: digits must follow the decimal point", s)
	}
	digits := cmp.Or(whole, "0") + frac + strings.Repeat("0", 6-len(frac))
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("amount %q: must be a non-negative number of dollars, for example 0.15", s)
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q: too large", s)
	}
	return MicroUSD(n), nil
}

// String prints the amount in dollars with six decimal places, for example "0.150000".
func (m MicroUSD) String() string {
	digits := strconv.FormatInt(int64(m), 10)
	sign, digits := "", strings.TrimPrefix(digits, "-")
	if m < 0 {
		sign = "-"
	}
	if len(digits) < 7 {
		digits = strings.Repeat("0", 7-len(digits)) + digits
	}
	return sign + digits[:len(digits)-6] + "." + digits[len(digits)-6:]
}
