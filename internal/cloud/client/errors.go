package client

import (
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
)

// IsNotFound reports whether err is (or wraps) an OpenStack 404 — used to tell "the object is gone"
// apart from a transient failure (e.g. the os-notification re-fetch resolving a DELETE).
func IsNotFound(err error) bool {
	return err != nil && gophercloud.ResponseCodeIs(err, http.StatusNotFound)
}

// IsConflict reports whether err is (or wraps) an OpenStack 409 — "it already exists", which
// ensure-shaped calls treat as success.
func IsConflict(err error) bool {
	return err != nil && gophercloud.ResponseCodeIs(err, http.StatusConflict)
}
