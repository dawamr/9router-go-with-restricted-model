package media

import (
	"bytes"
	"encoding/base64"
	json "encoding/json/v2"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"9router/proxy/internal/handlers/chat"
	"9router/proxy/internal/log"
	"9router/proxy/internal/models"
	"9router/proxy/internal/providers"
	"9router/proxy/internal/usagetracker"
)

// cloudflareImageBaseURL is the Workers AI REST root. The account id and model
// complete it into /accounts/{accountId}/ai/run/{model}. Cloudflare serves
// Workers AI there; it has no /ai/v1/images/generations route, so the OpenAI
// passthrough path can never reach an image model. Upstream parity:
// registry cloudflare-ai.js imageConfig.baseUrl + imageProviders/cloudflareAi.js.
const cloudflareImageBaseURL = "https://api.cloudflare.com/client/v4/accounts"

// cloudflareMultipartModels need multipart/form-data rather than JSON — the
// FLUX.2 family rejects a JSON body with "required properties at '/' are
// 'multipart'" (upstream MULTIPART_MODELS).
var cloudflareMultipartModels = map[string]bool{
	"@cf/black-forest-labs/flux-2-dev":      true,
	"@cf/black-forest-labs/flux-2-klein-4b": true,
	"@cf/black-forest-labs/flux-2-klein-9b": true,
}

// cloudflareImageRequest is the OpenAI image request plus the extra fields
// Workers AI accepts. Optional numbers are pointers so an absent field stays
// absent instead of being sent as a zero.
type cloudflareImageRequest struct {
	Prompt         string   `json:"prompt"`
	Size           string   `json:"size"`
	Width          int      `json:"width"`
	Height         int      `json:"height"`
	NegativePrompt string   `json:"negative_prompt"`
	Guidance       *float64 `json:"guidance"`
	Seed           *int     `json:"seed"`
	NumSteps       *int     `json:"num_steps"`
	Steps          *int     `json:"steps"`
	Strength       *float64 `json:"strength"`
	Image          string   `json:"image"`
	Images         []string `json:"images"`
	MaskImage      string   `json:"mask_image"`
	MaskImageCamel string   `json:"maskImage"`
	Mask           string   `json:"mask"`
}

// isCloudflareImageEndpoint reports whether the endpoint is an image generation
// route.
func isCloudflareImageEndpoint(endpoint string) bool {
	return endpoint == "/v1/images/generations" || endpoint == "/images/generations"
}

// isCloudflareAIProvider reports whether a provider id or alias names Cloudflare
// Workers AI. ResolveAlias is the single place aliases are mapped, so `cf` and
// `cloudflare-ai` both answer without a hardcoded alias check.
func isCloudflareAIProvider(provider string) bool {
	return providers.ResolveAlias(provider) == "cloudflare-ai"
}

// handleCloudflareImage serves /v1/images/generations for Cloudflare Workers AI
// by translating the OpenAI request to the Workers AI REST shape and normalizing
// the response back. Upstream parity: imageProviders/cloudflareAi.js.
func (h *MediaHandler) handleCloudflareImage(w http.ResponseWriter, r *http.Request, body []byte, modelInfo *chat.ModelInfo) error {
	pinned := modelInfo.ConnectionID
	if pinned == "" {
		pinned = imagePinnedConnectionID(r)
	}
	usePinned := pinned != ""
	var excludeIDs []string
	var lastErr error
	for {
		conn, connData, err := h.ChatH.GetBestConnection("cloudflare-ai", pinned, excludeIDs, modelInfo.Model)
		if err != nil || conn == nil {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("no active connection for cloudflare-ai: %w", err)
		}
		attemptErr := h.tryCloudflareImageConn(w, r, body, modelInfo, conn, connData)
		if attemptErr == nil {
			return nil
		}
		lastErr = attemptErr
		if usePinned {
			return lastErr
		}
		excludeIDs = append(excludeIDs, conn.ID)
	}
}

func (h *MediaHandler) tryCloudflareImageConn(w http.ResponseWriter, r *http.Request, body []byte, modelInfo *chat.ModelInfo, conn *models.ProviderConnection, connData *chat.ConnectionData) error {
	accountID := chat.ProviderSpecificDataString(connData, "accountId", "account_id")
	if accountID == "" {
		return fmt.Errorf("cloudflare-ai requires accountId in providerSpecificData")
	}
	apiKey := chat.ExtractAPIKey(connData)
	if apiKey == "" {
		return fmt.Errorf("no API key found for cloudflare-ai connection %s", conn.ID)
	}

	var reqBody cloudflareImageRequest
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return fmt.Errorf("invalid image request: %w", err)
	}
	if strings.TrimSpace(reqBody.Prompt) == "" {
		return fmt.Errorf("Missing required field: prompt")
	}

	model := strings.TrimPrefix(modelInfo.Model, "cf/")
	payload, contentType, err := buildCloudflareImageBody(model, &reqBody)
	if err != nil {
		return fmt.Errorf("build cloudflare image body: %w", err)
	}

	targetURL := cloudflareRunURL(accountID, model)
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	client, err := h.ChatH.GetClientForConnection(connData)
	if err != nil {
		return err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("cloudflare image request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read cloudflare image response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		errText := string(respBody[:min(500, len(respBody))])
		log.Warn("media", "cloudflare image error", "status", resp.StatusCode, "body", errText)
		h.coolDownCloudflareConn(conn, modelInfo.Model, resp.StatusCode, errText)
		return fmt.Errorf("cloudflare image failed with status %d: %s", resp.StatusCode, string(respBody[:min(300, len(respBody))]))
	}

	normalized, err := cloudflareImageResponse(respBody, resp.Header.Get("Content-Type"))
	if err != nil {
		return fmt.Errorf("normalize cloudflare image response: %w", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(normalized)

	if h.Repo != nil {
		h.Repo.UpdateConnectionLastUsed(conn.ID)
	}
	usagetracker.GetTracker().PushRecent(usagetracker.RecentRequest{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Model:     model,
		Provider:  "cloudflare-ai",
		Status:    "ok",
	}, h.Repo)
	return nil
}

// coolDownCloudflareConn parks an account that answered with a rotatable error
// (quota, rate limit, auth) so the next request skips it, matching the sibling
// antigravity image adapter.
func (h *MediaHandler) coolDownCloudflareConn(conn *models.ProviderConnection, model string, status int, errText string) {
	if h.Repo == nil {
		return
	}
	backoff := h.Repo.GetConnectionBackoffLevel(conn.ID)
	classification := providers.ClassifyError(status, errText, backoff)
	if !classification.ShouldFallback {
		return
	}
	cooldownSec := max(classification.CooldownMs/1000, 1)
	_ = h.Repo.LockConnectionModel(conn.ID, model, cooldownSec, classification.NewBackoffLevel)
}

// cloudflareRunURL is the Workers AI invocation route. Cloudflare answers any
// other shape (an empty account segment, or the OpenAI
// /ai/v1/images/generations route) with 7003/7000 "no route for that URI".
func cloudflareRunURL(accountID, model string) string {
	return fmt.Sprintf("%s/%s/ai/run/%s", cloudflareImageBaseURL, accountID, model)
}

// buildCloudflareImageBody picks the wire shape for a model: multipart for the
// FLUX.2 family, JSON otherwise.
func buildCloudflareImageBody(model string, req *cloudflareImageRequest) ([]byte, string, error) {
	if cloudflareMultipartModels[model] {
		return buildCloudflareMultipartBody(req)
	}
	payload, err := buildCloudflareJSONBody(model, req)
	if err != nil {
		return nil, "", err
	}
	return payload, "application/json", nil
}

// cloudflareJSONFieldWhitelist restricts the JSON body to the fields each
// model's input schema actually accepts, confirmed against Cloudflare's own
// schema endpoint (GET /accounts/{id}/ai/models/schema?model=...). Workers AI
// rejects unknown properties outright (e.g. flux-1-schnell has no
// width/height/seed — only prompt and steps), so the field set is model-
// specific rather than a single generic shape. A model with no entry here
// (schema not reachable, e.g. the runwayml img2img family returns 403 on this
// account) falls back to sending every populated field, matching the prior
// behavior.
var cloudflareJSONFieldWhitelist = map[string]map[string]bool{
	"@cf/black-forest-labs/flux-1-schnell": cloudflareFieldSet(
		"prompt", "steps"),
	"@cf/bytedance/stable-diffusion-xl-lightning": cloudflareFieldSet(
		"prompt", "width", "height", "guidance", "negative_prompt", "seed", "num_steps", "strength", "image", "image_b64", "mask"),
	"@cf/leonardo/lucid-origin": cloudflareFieldSet(
		"prompt", "width", "height", "guidance", "seed", "num_steps", "steps"),
	"@cf/leonardo/phoenix-1.0": cloudflareFieldSet(
		"prompt", "width", "height", "guidance", "negative_prompt", "seed", "num_steps"),
	"@cf/lykon/dreamshaper-8-lcm": cloudflareFieldSet(
		"prompt", "width", "height", "guidance", "negative_prompt", "seed", "num_steps", "strength", "image", "image_b64", "mask"),
	"@cf/runwayml/stable-diffusion-v1-5-inpainting": cloudflareFieldSet(
		"prompt", "width", "height", "guidance", "negative_prompt", "seed", "num_steps", "strength", "image", "image_b64", "mask"),
	"@cf/stabilityai/stable-diffusion-xl-base-1.0": cloudflareFieldSet(
		"prompt", "width", "height", "guidance", "negative_prompt", "seed", "num_steps", "strength", "image", "image_b64", "mask"),
}

func cloudflareFieldSet(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// buildCloudflareJSONBody mirrors upstream buildJsonBody: prompt plus any
// dimensions and optional fields, then base64 image/mask inputs — filtered
// down to whatever the model's own schema accepts (cloudflareJSONFieldWhitelist).
func buildCloudflareJSONBody(model string, req *cloudflareImageRequest) ([]byte, error) {
	payload := map[string]any{"prompt": req.Prompt}
	for k, v := range cloudflareDimensions(req) {
		payload[k] = v
	}
	if req.NegativePrompt != "" {
		payload["negative_prompt"] = req.NegativePrompt
	}
	if req.Guidance != nil {
		payload["guidance"] = *req.Guidance
	}
	if req.Seed != nil {
		payload["seed"] = *req.Seed
	}
	if req.NumSteps != nil {
		payload["num_steps"] = *req.NumSteps
	}
	if req.Steps != nil {
		payload["steps"] = *req.Steps
	}
	if req.Strength != nil {
		payload["strength"] = *req.Strength
	}

	if b64, raw := cloudflareImageInput(firstNonEmptyString(req.Image, firstString(req.Images))); b64 != "" {
		payload["image_b64"] = b64
		payload["image"] = raw
	}
	if b64, raw := cloudflareImageInput(firstNonEmptyString(req.MaskImage, req.MaskImageCamel, req.Mask)); b64 != "" {
		payload["mask_b64"] = b64
		payload["mask"] = raw
		payload["mask_image"] = raw
	}

	if allowed := cloudflareJSONFieldWhitelist[model]; allowed != nil {
		for k := range payload {
			if !allowed[k] {
				delete(payload, k)
			}
		}
	}
	return json.Marshal(payload)
}

// buildCloudflareMultipartBody mirrors upstream buildMultipartBody: prompt,
// dimensions and optional fields as text parts.
func buildCloudflareMultipartBody(req *cloudflareImageRequest) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("prompt", req.Prompt); err != nil {
		return nil, "", err
	}
	for k, v := range cloudflareDimensions(req) {
		if err := mw.WriteField(k, strconv.Itoa(v)); err != nil {
			return nil, "", err
		}
	}
	for _, f := range cloudflareOptionalFields(req) {
		if err := mw.WriteField(f.key, f.value); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

type cloudflareField struct{ key, value string }

func cloudflareOptionalFields(req *cloudflareImageRequest) []cloudflareField {
	var out []cloudflareField
	if req.NegativePrompt != "" {
		out = append(out, cloudflareField{"negative_prompt", req.NegativePrompt})
	}
	if req.Guidance != nil {
		out = append(out, cloudflareField{"guidance", strconv.FormatFloat(*req.Guidance, 'f', -1, 64)})
	}
	if req.Seed != nil {
		out = append(out, cloudflareField{"seed", strconv.Itoa(*req.Seed)})
	}
	if req.NumSteps != nil {
		out = append(out, cloudflareField{"num_steps", strconv.Itoa(*req.NumSteps)})
	}
	if req.Steps != nil {
		out = append(out, cloudflareField{"steps", strconv.Itoa(*req.Steps)})
	}
	if req.Strength != nil {
		out = append(out, cloudflareField{"strength", strconv.FormatFloat(*req.Strength, 'f', -1, 64)})
	}
	return out
}

// cloudflareDimensions derives width/height from an explicit size ("1024x1024")
// or explicit width/height fields, matching upstream getDimensions.
func cloudflareDimensions(req *cloudflareImageRequest) map[string]int {
	out := map[string]int{}
	if w, h, ok := parseImageSize(req.Size); ok {
		out["width"] = w
		out["height"] = h
	}
	if req.Width > 0 {
		out["width"] = req.Width
	}
	if req.Height > 0 {
		out["height"] = req.Height
	}
	return out
}

// parseImageSize parses an "NxN" size string. Anything else ("auto") is ignored.
func parseImageSize(size string) (int, int, bool) {
	parts := strings.SplitN(strings.TrimSpace(size), "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errW != nil || errH != nil {
		return 0, 0, false
	}
	return w, h, true
}

// cloudflareImageInput turns an inline image (raw base64 or a data: URI) into the
// base64 string and byte array Cloudflare expects. A remote URL is not fetched —
// the gateway does not retrieve network images here — and is dropped so the
// request is still sent.
func cloudflareImageInput(value string) (string, []int) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", nil
	}
	lower := strings.ToLower(v)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return "", nil
	}
	if strings.HasPrefix(lower, "data:image/") {
		if i := strings.Index(lower, "base64,"); i >= 0 {
			v = v[i+len("base64,"):]
		}
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", nil
	}
	bytesArr := make([]int, len(raw))
	for i, b := range raw {
		bytesArr[i] = int(b)
	}
	return v, bytesArr
}

// cloudflareImageResponse normalizes a Workers AI image reply. Workers AI may
// answer a JSON envelope ({"result":{"image":"<base64>"}}), an already-OpenAI
// body, or raw image bytes; all three become {created, data:[...]}.
func cloudflareImageResponse(raw []byte, contentType string) ([]byte, error) {
	if strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return wrapCloudflareImageItems(map[string]any{"b64_json": base64.StdEncoding.EncodeToString(raw)}), nil
	}
	return normalizeCloudflareImageResponse(raw)
}

// normalizeCloudflareImageResponse mirrors upstream normalizeCloudflareResponse.
func normalizeCloudflareImageResponse(raw []byte) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		// Not JSON — Cloudflare returned a bare base64 image string.
		return wrapCloudflareImageItems(cloudflareImageItem(strings.TrimSpace(string(raw)))), nil
	}

	if _, ok := body["created"]; ok {
		if data, ok := body["data"].([]any); ok && len(data) > 0 {
			return raw, nil
		}
	}

	result, _ := body["result"].(map[string]any)
	if result == nil {
		if s, ok := body["result"].(string); ok {
			return wrapCloudflareImageItems(cloudflareImageItem(s)), nil
		}
		result = body
	}

	// Queued/batch responses: first entry whose success is not explicitly false.
	if responses, ok := result["responses"].([]any); ok {
		for _, entry := range responses {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if success, ok := m["success"].(bool); ok && !success {
				continue
			}
			if nested, ok := m["result"]; ok {
				return normalizeNestedCloudflareResult(nested)
			}
		}
	}

	var image any
	switch {
	case result["image"] != nil:
		image = result["image"]
	case result["data"] != nil:
		if arr, ok := result["data"].([]any); ok && len(arr) > 0 {
			if m, ok := arr[0].(map[string]any); ok {
				if m["b64_json"] != nil {
					image = m["b64_json"]
				} else if m["url"] != nil {
					image = m["url"]
				}
			}
		}
	}
	return wrapCloudflareImageItems(cloudflareImageItem(asString(image))), nil
}

func normalizeNestedCloudflareResult(nested any) ([]byte, error) {
	b, err := json.Marshal(map[string]any{"result": nested})
	if err != nil {
		return nil, err
	}
	return normalizeCloudflareImageResponse(b)
}

// cloudflareImageItem converts an image value into an OpenAI data item: a
// data: URI becomes b64_json, an http(s) URL becomes url, anything else is
// treated as bare base64.
func cloudflareImageItem(value string) map[string]any {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	lower := strings.ToLower(v)
	if strings.HasPrefix(lower, "data:image/") {
		if i := strings.Index(lower, "base64,"); i >= 0 {
			return map[string]any{"b64_json": v[i+len("base64,"):]}
		}
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return map[string]any{"url": v}
	}
	return map[string]any{"b64_json": v}
}

func asString(value any) string {
	s, _ := value.(string)
	return s
}

func wrapCloudflareImageItems(item map[string]any) []byte {
	data := []any{}
	if item != nil {
		data = append(data, item)
	}
	out, _ := json.Marshal(map[string]any{"created": time.Now().Unix(), "data": data})
	return out
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstString(values []string) string {
	if len(values) > 0 {
		return values[0]
	}
	return ""
}
