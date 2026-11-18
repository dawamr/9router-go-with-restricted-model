package handlerutil

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWantsRawImage(t *testing.T) {
	tests := []struct {
		name   string
		url    string
		accept string
		want   bool
	}{
		{
			name: "no opt-in signals defaults to JSON",
			url:  "/v1/images/generations",
			want: false,
		},
		{
			// Upstream parity: decolua/9router GenericExampleCard.js builds
			// `apiPath + "?response_format=binary"` and reads the result by
			// checking the response content-type, not by sending any
			// special Accept header.
			name: "response_format=binary opts in (upstream dashboard contract)",
			url:  "/v1/images/generations?response_format=binary",
			want: true,
		},
		{
			name: "response_format=json stays JSON",
			url:  "/v1/images/generations?response_format=json",
			want: false,
		},
		{
			name: "raw=1 query opts in",
			url:  "/v1/images/generations?raw=1",
			want: true,
		},
		{
			name: "raw=true query opts in",
			url:  "/v1/images/generations?raw=true",
			want: true,
		},
		{
			name: "format=binary query opts in",
			url:  "/v1/images/generations?format=binary",
			want: true,
		},
		{
			name:   "Accept: image/png opts in",
			url:    "/v1/images/generations",
			accept: "image/png",
			want:   true,
		},
		{
			name:   "Accept: image/* opts in",
			url:    "/v1/images/generations",
			accept: "image/*",
			want:   true,
		},
		{
			// The default Accept header of virtually every HTTP client and
			// every OpenAI SDK call. It must never flip the documented JSON
			// contract — that would break every existing integration.
			name:   "bare Accept: */* never opts in",
			url:    "/v1/images/generations",
			accept: "*/*",
			want:   false,
		},
		{
			name:   "Accept: application/json, image/webp opts in on the image part",
			url:    "/v1/images/generations",
			accept: "application/json, image/webp;q=0.9",
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, tt.url, nil)
			if tt.accept != "" {
				r.Header.Set("Accept", tt.accept)
			}
			if got := WantsRawImage(r); got != tt.want {
				t.Errorf("WantsRawImage() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWantsRawImageNilRequest(t *testing.T) {
	if WantsRawImage(nil) {
		t.Error("WantsRawImage(nil) = true, want false")
	}
}

func TestRespondImage(t *testing.T) {
	// A 1x1 transparent PNG, the smallest valid PNG magic-byte payload.
	pngBytes := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	}
	pngB64 := base64.StdEncoding.EncodeToString(pngBytes)
	envelope := []byte(`{"created":1,"data":[{"b64_json":"` + pngB64 + `"}]}`)
	urlOnlyEnvelope := []byte(`{"created":1,"data":[{"url":"https://example.test/cat.png"}]}`)

	tests := []struct {
		name         string
		rawRequested bool
		status       int
		body         []byte
		wantContent  string // "json" or "binary"
	}{
		{
			name:         "default (no opt-in) stays JSON passthrough",
			rawRequested: false,
			status:       http.StatusOK,
			body:         envelope,
			wantContent:  "json",
		},
		{
			name:         "opt-in with a clean b64_json envelope decodes to binary",
			rawRequested: true,
			status:       http.StatusOK,
			body:         envelope,
			wantContent:  "binary",
		},
		{
			name:         "opt-in on a non-200 status stays JSON (error body)",
			rawRequested: true,
			status:       http.StatusBadGateway,
			body:         []byte(`{"error":{"message":"upstream failed"}}`),
			wantContent:  "json",
		},
		{
			name:         "opt-in with a url-only entry falls back to JSON",
			rawRequested: true,
			status:       http.StatusOK,
			body:         urlOnlyEnvelope,
			wantContent:  "json",
		},
		{
			name:         "opt-in with unparseable body falls back to JSON",
			rawRequested: true,
			status:       http.StatusOK,
			body:         []byte("not json at all"),
			wantContent:  "json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := "/v1/images/generations"
			if tt.rawRequested {
				url += "?response_format=binary"
			}
			r := httptest.NewRequest(http.MethodPost, url, nil)
			w := httptest.NewRecorder()

			RespondImage(w, r, tt.status, tt.body)

			ct := w.Header().Get("Content-Type")
			switch tt.wantContent {
			case "binary":
				if ct == "" || ct == "application/json" {
					t.Errorf("Content-Type = %q, want an image/* type", ct)
				}
				if w.Code != http.StatusOK {
					t.Errorf("status = %d, want 200", w.Code)
				}
				if string(w.Body.Bytes()) != string(pngBytes) {
					t.Errorf("body = %x, want decoded PNG bytes %x", w.Body.Bytes(), pngBytes)
				}
			case "json":
				if ct != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", ct)
				}
				if w.Code != tt.status {
					t.Errorf("status = %d, want %d", w.Code, tt.status)
				}
				if string(w.Body.Bytes()) != string(tt.body) {
					t.Errorf("body was rewritten: %s", w.Body.Bytes())
				}
			}
		})
	}
}
