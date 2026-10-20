package billing

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"

	"github.com/zsoftly/zs-core/internal/killbill"
)

// Money is an amount in minor units of a currency, such as cents.
type Money struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
}

// minorDigits is the number of minor-unit digits of each ISO 4217 currency
// Core accepts. Kill Bill amounts are decimals; Core works in minor units.
var minorDigits = map[string]int{
	"AUD": 2, "BRL": 2, "CAD": 2, "CHF": 2, "CNY": 2, "CZK": 2, "DKK": 2, "EUR": 2, "GBP": 2,
	"HKD": 2, "INR": 2, "MXN": 2, "NOK": 2, "NZD": 2, "PLN": 2, "SEK": 2, "SGD": 2, "USD": 2,
	"XOF": 0, "XAF": 0, "JPY": 0, "KRW": 0, "CLP": 0, "ISK": 0, "VND": 0,
	"BHD": 3, "JOD": 3, "KWD": 3, "OMR": 3, "TND": 3,
	"NGN": 2, "GHS": 2, "KES": 2, "ZAR": 2, "MAD": 2, "EGP": 2,
}

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// MinorDigits returns the number of minor-unit digits of a currency, or
// false for a currency Core does not know.
func MinorDigits(currency string) (int, bool) {
	d, ok := minorDigits[currency]
	return d, ok
}

func checkCurrency(currency string) error {
	if !currencyPattern.MatchString(currency) {
		return validation("The currency must be an ISO 4217 code in upper case, such as CAD.")
	}
	if _, ok := minorDigits[currency]; !ok {
		return validation("The currency " + currency + " is not supported.")
	}
	return nil
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// ToMinor converts a Kill Bill decimal amount to minor units of currency. A
// value with more digits than the currency has is rounded half away from
// zero. An empty value is zero.
func ToMinor(v json.Number, currency string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	digits, ok := minorDigits[currency]
	if !ok {
		return 0, fmt.Errorf("billing: currency %q is not supported", currency)
	}
	r, ok := new(big.Rat).SetString(string(v))
	if !ok {
		return 0, fmt.Errorf("billing: amount is not a decimal")
	}
	r.Mul(r, new(big.Rat).SetInt(pow10(digits)))
	num, den := new(big.Int).Set(r.Num()), r.Denom()
	neg := num.Sign() < 0
	num.Abs(num)
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(rem, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("billing: amount is out of range")
	}
	return q.Int64(), nil
}

// FromMinor converts minor units of currency to the decimal Kill Bill takes.
func FromMinor(amount int64, currency string) (json.Number, error) {
	digits, ok := minorDigits[currency]
	if !ok {
		return "", fmt.Errorf("billing: currency %q is not supported", currency)
	}
	r := new(big.Rat).SetFrac(big.NewInt(amount), pow10(digits))
	return json.Number(r.FloatString(digits)), nil
}

func money(v json.Number, currency string) (Money, error) {
	m, err := ToMinor(v, currency)
	if err != nil {
		return Money{}, err
	}
	return Money{Currency: currency, AmountMinor: m}, nil
}

// prices converts catalogue prices, leaving out currencies Core does not
// know.
func prices(list []killbill.Price) ([]Money, error) {
	out := make([]Money, 0, len(list))
	for _, p := range list {
		if _, ok := minorDigits[p.Currency]; !ok {
			continue
		}
		m, err := money(p.Value, p.Currency)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
