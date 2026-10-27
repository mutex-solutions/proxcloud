package billing

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

// currencyCodesJSON is the currency-codes dataset, embedded verbatim so getCurrencies
// derives consistent results.
//
//go:embed currency-codes.json
var currencyCodesJSON []byte

// CountryCurrency is a country's currency. The served keys are
// snake_case (currency_name/currency_code/numeric_code).
// numericCode is a STRING (the JSON number is coerced).
type CountryCurrency struct {
	Country      string `json:"country"`
	CurrencyName string `json:"currency_name"`
	CurrencyCode string `json:"currency_code"`
	NumericCode  string `json:"numeric_code"`
}

var (
	currenciesOnce sync.Once
	currencies     []CountryCurrency
)

// Currencies returns the currency list, DEDUPED by currencyCode
// (distinctByKey — first occurrence wins, source order preserved). Parsed once from the embedded
// dataset. numeric_code (a JSON number in the file) is rendered as a quoted string.
func Currencies() []CountryCurrency {
	currenciesOnce.Do(func() {
		var raw []struct {
			Country      string          `json:"country"`
			CurrencyName string          `json:"currency_name"`
			CurrencyCode string          `json:"currency_code"`
			NumericCode  json.RawMessage `json:"numeric_code"`
		}
		_ = json.Unmarshal(currencyCodesJSON, &raw)
		seen := make(map[string]bool, len(raw))
		currencies = make([]CountryCurrency, 0, len(raw))
		for _, c := range raw {
			if seen[c.CurrencyCode] { // distinctByKey(currencyCode)
				continue
			}
			seen[c.CurrencyCode] = true
			nc := strings.Trim(string(c.NumericCode), `"`)
			if nc == "null" {
				nc = ""
			}
			currencies = append(currencies, CountryCurrency{
				Country: c.Country, CurrencyName: c.CurrencyName, CurrencyCode: c.CurrencyCode, NumericCode: nc,
			})
		}
	})
	return currencies
}
