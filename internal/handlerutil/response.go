package handlerutil

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"9router/proxy/internal/constants"
)

// deterministicJSON makes Go maps serialize with their keys in sorted order, so
// the same data always produces byte-identical JSON. Without it, encoding/json/v2
// marshals maps in the runtime's randomized iteration order, which makes list
// order shift between requests (e.g. the dashboard models list on every refresh).
var deterministicJSON = json.Deterministic(true)

// errorTypes maps HTTP status codes to OpenAI-compatible error types and codes.
var errorTypes = map[int]struct {
	errType string
	errCode string
}{
	http.StatusBadRequest:          {errType: "invalid_request_error", errCode: "bad_request"},
	http.StatusUnauthorized:        {errType: "authentication_error", errCode: "invalid_api_key"},
	http.StatusPaymentRequired:     {errType: "billing_error", errCode: "payment_required"},
	http.StatusForbidden:           {errType: "permission_error", errCode: "insufficient_quota"},
	http.StatusNotFound:            {errType: "invalid_request_error", errCode: "model_not_found"},
	http.StatusMethodNotAllowed:    {errType: "invalid_request_error", errCode: "method_not_allowed"},
	http.StatusNotAcceptable:       {errType: "invalid_request_error", errCode: "model_not_supported"},
	http.StatusTooManyRequests:     {errType: "rate_limit_error", errCode: "rate_limit_exceeded"},
	http.StatusInternalServerError: {errType: "server_error", errCode: "internal_server_error"},
	http.StatusBadGateway:          {errType: "server_error", errCode: "bad_gateway"},
	http.StatusServiceUnavailable:  {errType: "server_error", errCode: "service_unavailable"},
	http.StatusGatewayTimeout:      {errType: "server_error", errCode: "gateway_timeout"},
}

// WriteJSONError writes a standardized JSON error response with status-code-aware
// error types matching OpenAI API conventions.
func WriteJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(status)

	errType := "invalid_request_error"
	errCode := fmt.Sprintf("%d", status)
	if t, ok := errorTypes[status]; ok {
		errType = t.errType
		errCode = t.errCode
	}

	errResp := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    errCode,
		},
	}
	if err := json.MarshalWrite(w, errResp, deterministicJSON); err != nil {
		w.Write([]byte(`{"error":{"message":"internal error","type":"server_error","code":"internal_server_error"}}`))
	}
}

// wantsRawImageQueryKeys are the query parameters that opt an
// /v1/images/generations caller into a raw binary response instead of the
// OpenAI-shaped {data:[{b64_json|url}]} envelope. response_format=binary is
// the exact contract the upstream dashboard playground uses (decolua/9router
// GenericExampleCard.js: `apiPath + (wantBinary ? "?response_format=binary" : "")`,
// detected on the client by `content-type` starting with "image/"); raw=1 and
// format=binary are accepted as more convenient aliases for curl use.
var wantsRawImageQueryKeys = []string{"response_format", "raw", "format"}

// WantsRawImage reports whether the caller asked for the raw image bytes
// instead of the OpenAI-shaped JSON envelope: either an explicit
// ?response_format=binary query flag (upstream parity, see
// wantsRawImageQueryKeys) or an Accept header that names an image type
// specifically (never a bare "*/*" — that is every HTTP client's default and
// must not silently change the wire contract). Opt-in only: the default
// stays the documented JSON contract AGENTS.md §1 requires for every /v1/*
// route, so existing clients parsing b64_json never see a behavior change.
func WantsRawImage(r *http.Request) bool {
	if r == nil {
		return false
	}
	q := r.URL.Query()
	for _, key := range wantsRawImageQueryKeys {
		switch strings.ToLower(q.Get(key)) {
		case "1", "true", "binary", "raw":
			return true
		}
	}
	for _, accept := range r.Header.Values("Accept") {
		for _, part := range strings.Split(accept, ",") {
			part = strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
			if part != "" && part != "*/*" && strings.HasPrefix(part, "image/") {
				return true
			}
		}
	}
	return false
}

// imageEnvelope is the minimal OpenAI /v1/images/generations response shape:
// {"created":..., "data":[{"b64_json":"...", "url":"..."}]}. Every provider
// adapter in this repo normalizes to exactly this shape before responding, so
// decoding it back out here never needs provider-specific knowledge.
type imageEnvelope struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
		URL     string `json:"url"`
	} `json:"data"`
}

// sniffImageContentType returns an image/* MIME type from the leading bytes,
// falling back to image/png (Workers AI and most generators default there)
// when the magic bytes are not recognized rather than guessing wrong.
func sniffImageContentType(data []byte) string {
	if ct := http.DetectContentType(data); strings.HasPrefix(ct, "image/") {
		return ct
	}
	return "image/png"
}

// RespondImage writes an /v1/images/generations response. When the caller
// opted into raw bytes via WantsRawImage, it decodes the first image out of
// the normalized {data:[{b64_json|url}]} envelope and writes it as a binary
// image/* body instead of JSON — so opening the endpoint directly in a
// browser or an `curl -o out.png` shows the picture, not a base64 blob. Any
// condition that is not a clean single-image JSON body (parse failure, a
// provider error payload, a url-only entry, zero images) falls back to
// writing respBody byte-for-byte unmodified: the documented JSON contract is
// always the safe default, never silently downgraded to an error page.
func RespondImage(w http.ResponseWriter, r *http.Request, status int, respBody []byte) {
	if status != http.StatusOK || !WantsRawImage(r) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
		return
	}
	var env imageEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil || len(env.Data) == 0 {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
		return
	}
	entry := env.Data[0]
	if entry.B64JSON == "" {
		// url-only entry (e.g. DALL-E default response_format): nothing to
		// decode locally without an extra upstream fetch, so stay on the
		// documented JSON contract rather than redirecting or proxying.
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(entry.B64JSON)
	if err != nil {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
		return
	}
	w.Header().Set(constants.HeaderContentType, sniffImageContentType(raw))
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// WriteJSON writes a JSON response directly to the ResponseWriter with zero intermediate byte buffering.
func WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(status)
	if err := json.MarshalWrite(w, data, deterministicJSON); err != nil {
		w.Write([]byte(`{"error":{"message":"internal error","type":"invalid_request_error","code":500}}`))
	}
}

// WriteJSONIndented writes a JSON response indented with two spaces — the shape
// the upstream dashboard's backup download produces with JSON.stringify(payload,
// null, 2). A handler that hands the body to the user as a file uses it so the
// saved backup stays human-readable.
func WriteJSONIndented(w http.ResponseWriter, status int, data any) {
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(status)
	if err := json.MarshalWrite(w, data, deterministicJSON, jsontext.WithIndent("  ")); err != nil {
		w.Write([]byte(`{"error":{"message":"internal error","type":"invalid_request_error","code":500}}`))
	}
}

func WriteModelsList(w http.ResponseWriter, status int, meta any, modelsJSON []byte) {
	metaJSON, err := json.Marshal(meta, deterministicJSON)
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, "failed to encode models")
		return
	}
	trimmed := bytes.TrimRight(metaJSON, " 	\r\n")
	if !bytes.HasSuffix(trimmed, []byte("}")) {
		WriteJSONError(w, http.StatusInternalServerError, "failed to encode models")
		return
	}
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(status)
	trimmed = trimmed[:len(trimmed)-1]
	_, _ = w.Write(trimmed)
	_, _ = w.Write([]byte(`,"data":`))
	_, _ = w.Write(modelsJSON)
	_, _ = w.Write([]byte(`,"models":`))
	_, _ = w.Write(modelsJSON)
	_, _ = w.Write([]byte(`}`))
}

// UpdateModelInBody returns a copy of body with the "model" field set to modelName.
func UpdateModelInBody(body []byte, modelName string) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body
	}
	m["model"] = modelName
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// SetAuthHeader applies the provider's auth scheme to the request.
func SetAuthHeader(req *http.Request, apiKey, authHeader, authScheme string) {
	if authHeader == "" {
		authHeader = "Authorization"
	}
	switch authScheme {
	case "bearer":
		req.Header.Set(authHeader, "Bearer "+apiKey)
	case "raw":
		req.Header.Set(authHeader, apiKey)
	default:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

// GetString safely extracts a string value from a map[string]any by key.
func GetString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// SessionHeaderKeys defines the client request header keys that carry session IDs in priority order.
var SessionHeaderKeys = []string{
	"x-claude-code-session-id",
	"x-session-id",
	"session-id",
	"session_id",
	"x-amp-thread-id",
}

// ExtractSessionID extracts the session identifier from incoming HTTP request headers.
func ExtractSessionID(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, key := range SessionHeaderKeys {
		if val := r.Header.Get(key); val != "" {
			return val
		}
	}
	return ""
}

type sessionIDKey struct{}

// WithSessionID returns a context carrying the given session ID.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" || ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// GetSessionID retrieves the session ID from the context if present.
func GetSessionID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if val, ok := ctx.Value(sessionIDKey{}).(string); ok {
		return val
	}
	return ""
}

type clientBetaKey struct{}

// WithClientAnthropicBeta carries the caller's own anthropic-beta flags on the
// context. They have to be merged into the upstream request rather than dropped:
// a client asking for a beta the gateway does not list would otherwise fail
// without ever being told why. (Upstream mergeAnthropicBeta, v0.5.91.)
func WithClientAnthropicBeta(ctx context.Context, beta string) context.Context {
	if beta == "" || ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, clientBetaKey{}, beta)
}

// GetClientAnthropicBeta returns the caller's anthropic-beta flags, or "".
func GetClientAnthropicBeta(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if val, ok := ctx.Value(clientBetaKey{}).(string); ok {
		return val
	}
	return ""
}
