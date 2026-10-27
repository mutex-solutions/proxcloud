package billing

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// countriesJSON is the restcountries dataset, embedded verbatim so the
// projection below derives consistent results.
//
//go:embed countries.json
var countriesJSON []byte

// Country is the projected billing country: name = name.common, cca2/cca3 alpha codes,
// ccn3 numeric. Matches the
// served shape {"name","cca2","cca3","ccn3"}.
type Country struct {
	Name    string `json:"name"`
	Alpha2  string `json:"cca2"`
	Alpha3  string `json:"cca3"`
	Numeric int    `json:"ccn3"`
}

var (
	countriesOnce sync.Once
	countries     []Country
)

// Countries returns the static billing country list,
// parsed once from the embedded dataset. ccn3 is a quoted string in the source
// (null for Kosovo) → coerced to int with 0 for null/non-numeric.
func Countries() []Country {
	countriesOnce.Do(func() {
		var raw []struct {
			Name struct {
				Common string `json:"common"`
			} `json:"name"`
			Cca2 string          `json:"cca2"`
			Cca3 string          `json:"cca3"`
			Ccn3 json.RawMessage `json:"ccn3"`
		}
		_ = json.Unmarshal(countriesJSON, &raw)
		countries = make([]Country, 0, len(raw))
		for _, c := range raw {
			s := strings.Trim(string(c.Ccn3), `"`)
			if s == "null" {
				s = ""
			}
			n, _ := strconv.Atoi(s)
			countries = append(countries, Country{Name: c.Name.Common, Alpha2: c.Cca2, Alpha3: c.Cca3, Numeric: n})
		}
		// Alphabetical by name — the dataset ships in an arbitrary order, and every
		// consumer (the billing country picker) wants it A→Z.
		sort.Slice(countries, func(i, j int) bool { return countries[i].Name < countries[j].Name })
	})
	return countries
}
