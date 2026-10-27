package pricing

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
)

// These implement the value coercions used by the
// rating operators (convertValueByType).

// toBool coerces to a boolean. bool as-is; "true"/"false" strings
// parsed. ok=false when it cannot convert.
func toBool(v any) (val bool, ok bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(strings.ToLower(x)))
		if err != nil {
			return false, false
		}
		return b, true
	}
	return false, false
}

// toString coerces to a string: strings as-is; bool→"true"/"false";
// numbers→their canonical string.
func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case decimal.Decimal:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(v)
	}
}

// toDecimal coerces to a decimal: numbers + numeric strings. Returns ok=false
// when it cannot convert.
func toDecimal(v any) (d decimal.Decimal, ok bool) {
	switch x := v.(type) {
	case decimal.Decimal:
		return x, true
	case int:
		return decimal.NewFromInt(int64(x)), true
	case int32:
		return decimal.NewFromInt(int64(x)), true
	case int64:
		return decimal.NewFromInt(x), true
	case float64:
		return decimal.NewFromFloat(x), true
	case float32:
		return decimal.NewFromFloat32(x), true
	case json.Number:
		dd, err := decimal.NewFromString(x.String())
		return dd, err == nil
	case string:
		dd, err := decimal.NewFromString(strings.TrimSpace(x))
		return dd, err == nil
	}
	return decimal.Zero, false
}

// toStringSlice coerces a list to []string.
func toStringSlice(vs []any) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, toString(v))
	}
	return out
}

// String comparison helpers matching Apache commons StringUtils (ASCII case-fold).
func equalsIgnoreCase(a, b string) bool { return strings.EqualFold(a, b) }
func containsIgnoreCase(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
func startsWithIgnoreCase(s, prefix string) bool {
	return strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix))
}
