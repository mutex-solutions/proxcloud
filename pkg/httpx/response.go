// Package httpx holds the HTTP wire contract: the CustomHttpResponse envelope,
// the central error model, and cursor pagination — all kept byte-compatible
// so the (unchanged) frontends keep working.
package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
)

// HTTPError is a domain error carrying the HTTP status + envelope code/message.
// Services return it; handlers map it via WriteError.
type HTTPError struct {
	Status int
	Code   int
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }

func NewError(status, code int, msg string) *HTTPError {
	return &HTTPError{Status: status, Code: code, Msg: msg}
}

// BadRequest/NotFound/Forbidden are HttpError factories (code == status).
func BadRequest(msg string) *HTTPError {
	return NewError(http.StatusBadRequest, http.StatusBadRequest, msg)
}
func NotFound(msg string) *HTTPError { return NewError(http.StatusNotFound, http.StatusNotFound, msg) }
func Forbidden(msg string) *HTTPError {
	return NewError(http.StatusForbidden, http.StatusForbidden, msg)
}

// WriteError writes a typed *HTTPError as the envelope and returns true; returns
// false if err is not an *HTTPError (caller should 500).
func WriteError(w http.ResponseWriter, err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		Err(w, he.Status, he.Code, he.Msg)
		return true
	}
	return false
}

// Response is the CustomHttpResponse<T> envelope. NOTE: `errors` is a single
// object {code,message}, not an array.
type Response[T any] struct {
	Data         *T        `json:"data,omitempty"`
	Errors       *APIError `json:"errors,omitempty"`
	AuthRedirect *string   `json:"authRedirect,omitempty"`
	RedirectURL  *string   `json:"redirectUrl,omitempty"`
	Paging       *Paging   `json:"paging,omitempty"`
}

// APIError is the per-error shape: a numeric code + an i18n message key.
type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Paging is the CustomHttpResponse.Paging shape. Pointers so offset/total=0
// are included (offset-list) while cursor fields stay omitted, and vice-versa.
type Paging struct {
	Limit      *int    `json:"limit,omitempty"`
	Offset     *int64  `json:"offset,omitempty"`
	Total      *int64  `json:"total,omitempty"`
	NextMarker *string `json:"nextMarker,omitempty"`
	PrevMarker *string `json:"prevMarker,omitempty"`
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// OK writes a 200 with a data envelope.
func OK[T any](w http.ResponseWriter, v T) {
	write(w, http.StatusOK, Response[T]{Data: &v})
}

// Empty writes a 200 with an empty envelope {} — CustomHttpResponse.single(null)
// (the null data field is omitted).
func Empty(w http.ResponseWriter) { write(w, http.StatusOK, Response[struct{}]{}) }

// Raw writes v as bare JSON (no CustomHttpResponse envelope) with the given status —
// for the handful of endpoints that return a plain object/map/list directly
// (account/details + /name, affiliate /check + /project/{id}/{config,log}).
func Raw(w http.ResponseWriter, status int, v any) { write(w, status, v) }

// Page writes a 200 with data + paging.
func Page[T any](w http.ResponseWriter, v T, p Paging) {
	write(w, http.StatusOK, Response[T]{Data: &v, Paging: &p})
}

// List writes the offset-list envelope: {data:[...], paging:{limit,offset,total}}
// — CustomHttpResponse.list(ListResponse.build(list)).
func List[T any](w http.ResponseWriter, items []T) {
	if items == nil {
		items = []T{}
	}
	limit := 50
	offset := int64(0)
	total := int64(len(items))
	write(w, http.StatusOK, Response[[]T]{Data: &items, Paging: &Paging{Limit: &limit, Offset: &offset, Total: &total}})
}

// Redirect writes a 200 with only a redirectUrl (CustomHttpResponse.redirect).
func Redirect(w http.ResponseWriter, url string) {
	write(w, http.StatusOK, Response[any]{RedirectURL: &url})
}

// CursorList writes the cursor-paged envelope: {data:[...], paging:{limit,
// offset:0, total:0, nextMarker?, prevMarker?}} — CustomHttpResponse.cursorList
// serializes offset+total as primitive 0 (only nulls are omitted); null markers
// are omitted.
func CursorList[T any](w http.ResponseWriter, items []T, limit int, nextMarker, prevMarker *string) {
	if items == nil {
		items = []T{}
	}
	l := limit
	off, tot := int64(0), int64(0)
	write(w, http.StatusOK, Response[[]T]{Data: &items, Paging: &Paging{Limit: &l, Offset: &off, Total: &tot, NextMarker: nextMarker, PrevMarker: prevMarker}})
}

// Accepted writes a 202 with no body.
func Accepted(w http.ResponseWriter) { w.WriteHeader(http.StatusAccepted) }

// Err writes an error envelope at the given HTTP status.
func Err(w http.ResponseWriter, status, code int, msgKey string) {
	write(w, status, Response[any]{Errors: &APIError{Code: code, Message: msgKey}})
}

// NotFoundHandler returns the error envelope for unmatched routes (so 404s
// carry the contract shape, not chi's plain-text default).
func NotFoundHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Err(w, http.StatusNotFound, http.StatusNotFound, "not_found")
	}
}

// MethodNotAllowedHandler returns the error envelope for 405s.
func MethodNotAllowedHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Err(w, http.StatusMethodNotAllowed, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}
