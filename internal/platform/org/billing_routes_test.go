package org

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The 2026-07-02 gap-scan routes must register (no chi pattern panics) and match their expected paths.
func TestBillingRoutes_gapScanAdditions(t *testing.T) {
	r := chi.NewRouter()
	(&BillingHandler{}).Routes(r)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/bill/bp1/download/b1/statement"},
		{http.MethodPost, "/kyc/p1"},
		// neighbours that must keep matching around the new statics:
		{http.MethodGet, "/bill/bp1/b1"},
		{http.MethodPost, "/payment/deposit/bp1"},
		{http.MethodGet, "/payment/bp1/gateway"},
	} {
		rctx := chi.NewRouteContext()
		if !r.Match(rctx, tc.method, tc.path) {
			t.Errorf("no route for %s %s", tc.method, tc.path)
		}
	}
}
