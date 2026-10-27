package project

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// cloudQuotaConflict turns Nova/Cinder/Neutron quota responses into the same
// client-correctable 409 shape as the ProxCloud GPU gate. The UI pre-check is a
// snapshot and can race another create, so the final provider response still
// needs to be understandable rather than falling through as internal.error.
func cloudQuotaConflict(err error) error {
	var responseErr gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &responseErr) {
		return nil
	}
	if responseErr.Actual != http.StatusForbidden &&
		responseErr.Actual != http.StatusConflict &&
		responseErr.Actual != http.StatusRequestEntityTooLarge {
		return nil
	}

	body := strings.ToLower(string(responseErr.Body))
	if !strings.Contains(body, "quota") &&
		!strings.Contains(body, "limitexceeded") &&
		!strings.Contains(body, "overlimit") &&
		!(strings.Contains(body, "maximum") && strings.Contains(body, "exceeded")) {
		return nil
	}

	message := providerErrorMessage(responseErr.Body)
	if message == "" {
		message = "Cloud quota exceeded. Refresh quota usage and choose a smaller configuration."
	}
	return httpx.NewError(http.StatusConflict, http.StatusConflict, message)
}

func providerErrorMessage(body []byte) string {
	var value any
	if len(body) == 0 || json.Unmarshal(body, &value) != nil {
		return ""
	}
	return nestedProviderMessage(value)
}

func nestedProviderMessage(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if message, ok := typed["message"].(string); ok {
			return strings.TrimSpace(message)
		}
		for _, nested := range typed {
			if message := nestedProviderMessage(nested); message != "" {
				return message
			}
		}
	case []any:
		for _, nested := range typed {
			if message := nestedProviderMessage(nested); message != "" {
				return message
			}
		}
	}
	return ""
}
