package media

import (
	"encoding/base64"
	json "encoding/json/v2"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"
)

// The Workers AI base URL and route shape: {baseURL}/{accountId}/ai/run/{model}.
// Upstream parity: imageProviders/cloudflareAi.js buildUrl.
func TestCloudflareImageTargetURLShape(t *testing.T) {
	tests := []struct {
		name      string
		accountID string
		model     string
		want     string
	}{
		{
			name:      "flux text-to-image model keeps its @cf path segment",
			accountID: "0123456789abcdef0123456789abcdef",
			model:     "@cf/black-forest-labs/flux-1-schnell",
			want:      "https://api.cloudflare.com/client/v4/accounts/0123456789abcdef0123456789abcdef/ai/run/@cf/black-forest-labs/flux-1-schnell",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cloudflareRunURL(tt.accountID, tt.model)
			if got != tt.want {
				t.Errorf("cloudflareRunURL() = %q, want %q", got, tt.want)
			}
			// The bug this guards: an empty account segment ("accounts//ai/run")
			// or the OpenAI passthrough route ("/ai/v1/images/generations"),
			// which Cloudflare answers with 7003/7000 "no route for that URI".
			if strings.Contains(got, "accounts//") {
				t.Errorf("cloudflareRunURL() has an empty account segment: %q", got)
			}
			if strings.Contains(got, "images/generations") {
				t.Errorf("cloudflareRunURL() must not use the OpenAI route: %q", got)
			}
		})
	}
}

func TestIsCloudflareAIProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		want     bool
	}{
		{name: "registry id", provider: "cloudflare-ai", want: true},
		{name: "ui alias", provider: "cf", want: true},
		{name: "sibling provider is not hijacked", provider: "antigravity", want: false},
		{name: "opencode stays opencode", provider: "opencode", want: false},
		{name: "empty", provider: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCloudflareAIProvider(tt.provider); got != tt.want {
				t.Errorf("isCloudflareAIProvider(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}

func TestIsCloudflareImageEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{name: "versioned image route", endpoint: "/v1/images/generations", want: true},
		{name: "bare image route", endpoint: "/images/generations", want: true},
		{name: "audio speech is not an image route", endpoint: "/v1/audio/speech", want: false},
		{name: "embeddings is not an image route", endpoint: "/v1/embeddings", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCloudflareImageEndpoint(tt.endpoint); got != tt.want {
				t.Errorf("isCloudflareImageEndpoint(%q) = %v, want %v", tt.endpoint, got, tt.want)
			}
		})
	}
}

func TestBuildCloudflareImageBodyJSON(t *testing.T) {
	tests := []struct {
		name        string
		model       string
		req         cloudflareImageRequest
		wantKeys    map[string]any
		absentKeys  []string
	}{
		{
			// Confirmed against Cloudflare's own schema endpoint:
			// flux-1-schnell accepts prompt + steps only. Sending
			// width/height/seed/num_steps fails with 5006
			// "Additional or unevaluated properties ... not allowed".
			name:  "flux-1-schnell keeps only prompt and steps per its schema",
			model: "@cf/black-forest-labs/flux-1-schnell",
			req: cloudflareImageRequest{
				Prompt:         "A cute cat wearing a hat",
				Size:           "1024x768",
				NegativePrompt: "dog",
				Guidance:       ptrFloat(7.5),
				Seed:           ptr(42),
				NumSteps:       ptr(8),
				Steps:          ptr(4),
				Image:          "data:image/png;base64,cG5n",
			},
			wantKeys: map[string]any{
				"prompt": "A cute cat wearing a hat",
				"steps":  float64(4),
			},
			absentKeys: []string{
				"width", "height", "seed", "num_steps", "negative_prompt",
				"guidance", "strength", "image", "image_b64", "mask", "mask_b64",
				"model", "n", "size", "quality", "background", "image_detail", "output_format",
			},
		},
		{
			name:  "size becomes width and height; OpenAI-only fields are dropped",
			model: "@cf/stabilityai/stable-diffusion-xl-base-1.0",
			req:   cloudflareImageRequest{Prompt: "A cute cat wearing a hat", Size: "1024x768"},
			wantKeys: map[string]any{
				"prompt": "A cute cat wearing a hat",
				"width":  float64(1024),
				"height": float64(768),
			},
			// Workers AI rejects these with 5006
			// "Additional or unevaluated properties ... not allowed".
			absentKeys: []string{"model", "n", "size", "quality", "background", "image_detail", "output_format"},
		},
		{
			name:       "auto size sends no dimensions",
			model:      "@cf/stabilityai/stable-diffusion-xl-base-1.0",
			req:        cloudflareImageRequest{Prompt: "cat", Size: "auto"},
			wantKeys:   map[string]any{"prompt": "cat"},
			absentKeys: []string{"width", "height", "model", "size"},
		},
		{
			name:  "explicit width and height override size",
			model: "@cf/stabilityai/stable-diffusion-xl-base-1.0",
			req:   cloudflareImageRequest{Prompt: "cat", Size: "1024x1024", Width: 512, Height: 256},
			wantKeys: map[string]any{
				"width":  float64(512),
				"height": float64(256),
			},
		},
		{
			name:  "optional sampling fields pass through",
			model: "@cf/stabilityai/stable-diffusion-xl-base-1.0",
			req: cloudflareImageRequest{
				Prompt:         "cat",
				NegativePrompt: "dog",
				Seed:           ptr(42),
				NumSteps:       ptr(8),
				Guidance:       ptrFloat(7.5),
				Strength:       ptrFloat(0.8),
			},
			wantKeys: map[string]any{
				"prompt":          "cat",
				"negative_prompt": "dog",
				"seed":            float64(42),
				"num_steps":       float64(8),
				"guidance":        7.5,
				"strength":        0.8,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, contentType, err := buildCloudflareImageBody(tt.model, &tt.req)
			if err != nil {
				t.Fatalf("buildCloudflareImageBody() error = %v", err)
			}
			if contentType != "application/json" {
				t.Fatalf("contentType = %q, want application/json", contentType)
			}
			var got map[string]any
			if err := json.Unmarshal(payload, &got); err != nil {
				t.Fatalf("payload is not JSON: %v (%s)", err, payload)
			}
			for k, want := range tt.wantKeys {
				if got[k] != want {
					t.Errorf("payload[%q] = %#v, want %#v", k, got[k], want)
				}
			}
			for _, k := range tt.absentKeys {
				if _, ok := got[k]; ok {
					t.Errorf("payload contains %q, which Workers AI rejects", k)
				}
			}
		})
	}
}

func TestBuildCloudflareImageBodyJSONImageInput(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	payload, _, err := buildCloudflareImageBody("@cf/lykon/dreamshaper-8-lcm", &cloudflareImageRequest{
		Prompt: "restyle",
		Image:  "data:image/png;base64," + raw,
	})
	if err != nil {
		t.Fatalf("buildCloudflareImageBody() error = %v", err)
	}
	var got struct {
		ImageB64 string `json:"image_b64"`
		Image    []int  `json:"image"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if got.ImageB64 != raw {
		t.Errorf("image_b64 = %q, want %q", got.ImageB64, raw)
	}
	if len(got.Image) != len("png-bytes") {
		t.Errorf("image byte array length = %d, want %d", len(got.Image), len("png-bytes"))
	}
}

func TestBuildCloudflareImageBodyMultipart(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{name: "flux-2-dev", model: "@cf/black-forest-labs/flux-2-dev"},
		{name: "flux-2-klein-4b", model: "@cf/black-forest-labs/flux-2-klein-4b"},
		{name: "flux-2-klein-9b", model: "@cf/black-forest-labs/flux-2-klein-9b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, contentType, err := buildCloudflareImageBody(tt.model, &cloudflareImageRequest{
				Prompt: "A cute cat wearing a hat",
				Size:   "1024x768",
			})
			if err != nil {
				t.Fatalf("buildCloudflareImageBody() error = %v", err)
			}
			mediaType, params, err := mime.ParseMediaType(contentType)
			if err != nil || mediaType != "multipart/form-data" {
				t.Fatalf("contentType = %q, want multipart/form-data (%v)", contentType, err)
			}
			mr := multipart.NewReader(strings.NewReader(string(payload)), params["boundary"])
			fields := map[string]string{}
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				data, err := io.ReadAll(part)
				if err != nil {
					t.Fatalf("read part %q: %v", part.FormName(), err)
				}
				fields[part.FormName()] = string(data)
			}
			if fields["prompt"] != "A cute cat wearing a hat" {
				t.Errorf("prompt = %q, want %q", fields["prompt"], "A cute cat wearing a hat")
			}
			if fields["width"] != "1024" || fields["height"] != "768" {
				t.Errorf("dimensions = %qx%q, want 1024x768", fields["width"], fields["height"])
			}
		})
	}
}

func TestNormalizeCloudflareImageResponse(t *testing.T) {
	tests := []struct {
		name            string
		raw             string
		contentType     string
		wantB64         string
		wantURL         string
		wantDataLen     int
		wantPassthrough bool
	}{
		{
			name:        "workers ai envelope result.image becomes b64_json",
			raw:         `{"result":{"image":"aGVsbG8="},"success":true,"errors":[],"messages":[]}`,
			wantB64:     "aGVsbG8=",
			wantDataLen: 1,
		},
		{
			name:        "data uri image is unwrapped to raw base64",
			raw:         `{"result":{"image":"data:image/png;base64,aGVsbG8="},"success":true}`,
			wantB64:     "aGVsbG8=",
			wantDataLen: 1,
		},
		{
			name:        "http image becomes url",
			raw:         `{"result":{"image":"https://example.test/cat.png"},"success":true}`,
			wantURL:     "https://example.test/cat.png",
			wantDataLen: 1,
		},
		{
			name:        "result given as a bare base64 string",
			raw:         `{"result":"aGVsbG8=","success":true}`,
			wantB64:     "aGVsbG8=",
			wantDataLen: 1,
		},
		{
			name:            "already OpenAI shaped body passes through untouched",
			raw:             `{"created":123,"data":[{"url":"https://example.test/cat.png"}]}`,
			wantURL:         "https://example.test/cat.png",
			wantDataLen:     1,
			wantPassthrough: true,
		},
		{
			name:        "queued response picks the first successful entry",
			raw:         `{"result":{"responses":[{"success":false,"result":{"image":"fail"}},{"success":true,"result":{"image":"aGVsbG8="}}]}}`,
			wantB64:     "aGVsbG8=",
			wantDataLen: 1,
		},
		{
			name:        "result.data array is honored",
			raw:         `{"result":{"data":[{"b64_json":"aGVsbG8="}]},"success":true}`,
			wantB64:     "aGVsbG8=",
			wantDataLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := cloudflareImageResponse([]byte(tt.raw), tt.contentType)
			if err != nil {
				t.Fatalf("cloudflareImageResponse() error = %v", err)
			}
			if tt.wantPassthrough {
				if string(out) != tt.raw {
					t.Errorf("body was rewritten: %s", out)
				}
			}
			var got struct {
				Created int64 `json:"created"`
				Data    []struct {
					B64JSON string `json:"b64_json"`
					URL     string `json:"url"`
				} `json:"data"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("normalized body is not JSON: %v (%s)", err, out)
			}
			if tt.wantDataLen > 0 && len(got.Data) != tt.wantDataLen {
				t.Fatalf("data length = %d, want %d (%s)", len(got.Data), tt.wantDataLen, out)
			}
			if tt.wantB64 != "" && got.Data[0].B64JSON != tt.wantB64 {
				t.Errorf("b64_json = %q, want %q", got.Data[0].B64JSON, tt.wantB64)
			}
			if tt.wantURL != "" && got.Data[0].URL != tt.wantURL {
				t.Errorf("url = %q, want %q", got.Data[0].URL, tt.wantURL)
			}
		})
	}
}

func TestCloudflareImageResponseRawImageBytes(t *testing.T) {
	raw := []byte{0x89, 0x50, 0x4e, 0x47}
	out, err := cloudflareImageResponse(raw, "image/png")
	if err != nil {
		t.Fatalf("cloudflareImageResponse() error = %v", err)
	}
	var got struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("normalized body is not JSON: %v", err)
	}
	if len(got.Data) != 1 || got.Data[0].B64JSON != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("data = %+v, want one b64_json item carrying the raw bytes", got.Data)
	}
}

func TestParseImageSize(t *testing.T) {
	tests := []struct {
		name   string
		size   string
		w, h   int
		wantOK bool
	}{
		{name: "canonical", size: "1024x1024", w: 1024, h: 1024, wantOK: true},
		{name: "non square", size: "768x1024", w: 768, h: 1024, wantOK: true},
		{name: "auto is not a size", size: "auto", wantOK: false},
		{name: "empty", size: "", wantOK: false},
		{name: "garbage", size: "wide", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, h, ok := parseImageSize(tt.size)
			if ok != tt.wantOK {
				t.Fatalf("parseImageSize(%q) ok = %v, want %v", tt.size, ok, tt.wantOK)
			}
			if ok && (w != tt.w || h != tt.h) {
				t.Errorf("parseImageSize(%q) = %dx%d, want %dx%d", tt.size, w, h, tt.w, tt.h)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func ptrFloat(v float64) *float64 { return &v }
