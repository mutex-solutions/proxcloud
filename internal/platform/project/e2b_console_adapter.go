package project

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// bufferedResponse delays publishing an internal handler's response until its
// transaction commits, or its wire contract has been translated. It deliberately
// implements only ResponseWriter: these internal JSON endpoints never stream.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: make(http.Header)}
}
func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.WriteHeader(http.StatusOK)
	return b.body.Write(p)
}
func (b *bufferedResponse) statusCode() int {
	if b.status == 0 {
		return http.StatusOK
	}
	return b.status
}
func (b *bufferedResponse) publish(w http.ResponseWriter) {
	for k, values := range b.header {
		w.Header()[k] = append([]string(nil), values...)
	}
	w.WriteHeader(b.statusCode())
	_, _ = w.Write(b.body.Bytes())
}

// consoleE2BRequest is an in-process adapter, NOT an HTTP/API-key authentication
// shortcut. Its caller must use the principal returned by consoleCaller. Replace
// any pre-existing E2B principal and route context; the authorized project comes
// exclusively from the console's project path, never from JSON or headers.
func consoleE2BRequest(r *http.Request, c *e2bCaller, templateID, buildID string, body []byte) *http.Request {
	rc := chi.NewRouteContext()
	rc.URLParams.Add("templateID", templateID)
	rc.URLParams.Add("buildID", buildID)
	ctx := context.WithValue(r.Context(), e2bCtxKey{}, c)
	adapted := r.Clone(context.WithValue(ctx, chi.RouteCtxKey, rc))
	adapted.Header.Del("X-API-Key")
	adapted.Header.Del("Authorization")
	if body != nil {
		adapted.Body = io.NopCloser(bytes.NewReader(body))
		adapted.ContentLength = int64(len(body))
	}
	return adapted
}

func captureConsoleE2B(r *http.Request, handler http.HandlerFunc) *bufferedResponse {
	response := newBufferedResponse()
	handler(response, r)
	return response
}

// writeConsoleE2B preserves admission/conflict/not-found status codes while
// translating the E2B {code,message} contract into the console's httpx envelope.
func writeConsoleE2B(w http.ResponseWriter, response *bufferedResponse) {
	status := response.statusCode()
	if status >= 400 {
		var failure httpx.APIError
		if json.Unmarshal(response.body.Bytes(), &failure) != nil || failure.Message == "" {
			failure.Message = "sandbox template request failed"
		}
		httpx.Err(w, status, status, failure.Message)
		return
	}
	var data any = map[string]any{}
	if response.body.Len() > 0 && json.Unmarshal(response.body.Bytes(), &data) != nil {
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, "invalid sandbox template response")
		return
	}
	// Console deletion has historically returned a 200 data envelope.
	if status == http.StatusNoContent {
		status = http.StatusOK
	}
	httpx.Raw(w, status, httpx.Response[any]{Data: &data})
}
