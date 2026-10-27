package billing

import (
	"github.com/nyaruka/phonenumbers"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// e164MobileNumber parses `mobile` with `region` as the default
// region, requires a valid + possible-MOBILE number, and returns the E.164 string.
// Both error messages are LITERAL (not translated). nyaruka/phonenumbers is a Go
// implementation of Google libphonenumber, so the formatted output is standard E.164.
func e164MobileNumber(mobile, region string) (string, error) {
	num, err := phonenumbers.Parse(mobile, region)
	if err != nil {
		return "", httpx.BadRequest("Error in parsing mobile number. The mobile number must be in E164 format")
	}
	// The canonical check is isValidNumber && isPossibleNumberForType(MOBILE). nyaruka v1.8.0
	// has no IsPossibleNumberForType, so we approximate with the number's actual
	// type — accepting MOBILE / FIXED_LINE_OR_MOBILE (correct for mobile
	// numbers; a fixed-line with mobile-compatible length is the only divergence).
	if phonenumbers.IsValidNumber(num) {
		switch phonenumbers.GetNumberType(num) {
		case phonenumbers.MOBILE, phonenumbers.FIXED_LINE_OR_MOBILE:
			return phonenumbers.Format(num, phonenumbers.E164), nil
		}
	}
	return "", httpx.BadRequest("Mobile number is invalid with the provided locale")
}
