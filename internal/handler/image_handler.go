package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aurora/httpclient"
	"aurora/httpclient/bogdanfinn"
	"aurora/internal/accounts"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	"aurora/internal/imageflow"
	officialtypes "aurora/typings/official"

	"github.com/gin-gonic/gin"
	_ "golang.org/x/image/webp"
)

type ImageHandler struct {
	accountPool *accounts.Pool
	cfg         *config.Config
}

const (
	imageUpstreamRequestTimeoutSeconds       = 90
	maxImageEditSources                      = 16
	maxImageEditTotalBytes             int64 = 40 << 20
	imageUploadMaxAttempts                   = 3
)

func NewImageHandler(pool *accounts.Pool, cfg *config.Config) *ImageHandler {
	return &ImageHandler{accountPool: pool, cfg: cfg}
}

func setupImageClientWithProxy(proxyURL string) *bogdanfinn.TlsClient {
	client := bogdanfinn.NewStdClientWithTimeout(imageUpstreamRequestTimeoutSeconds)
	if proxyURL != "" {
		_ = client.SetProxy(proxyURL)
	}
	return client
}

// ─── Image stream types ──────────────────────────────────────────

type imageStreamChunk struct {
	Object            string `json:"object"`
	Index             int    `json:"index"`
	Total             int    `json:"total"`
	Created           int64  `json:"created"`
	ProgressText      string `json:"progress_text,omitempty"`
	UpstreamEventType string `json:"upstream_event_type,omitempty"`
	Model             string `json:"model,omitempty"`
	AccountEmail      string `json:"_account_email,omitempty"`
	ConversationID    string `json:"_conversation_id,omitempty"`
}

type imageStreamResult struct {
	Object  string                              `json:"object"`
	Index   int                                 `json:"index"`
	Total   int                                 `json:"total"`
	Created int64                               `json:"created"`
	Model   string                              `json:"model,omitempty"`
	Data    []officialtypes.ImageGenerationData `json:"data"`
}

type imageStreamCompleted struct {
	Object  string                              `json:"object"`
	Created int64                               `json:"created"`
	Model   string                              `json:"model,omitempty"`
	Data    []officialtypes.ImageGenerationData `json:"data"`
}

func writeImageStreamHeader(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(200)
}

func writeImageStreamEvent(c *gin.Context, event string, payload interface{}) bool {
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if event != "" {
		if _, err := c.Writer.WriteString("event: " + event + "\n"); err != nil {
			return false
		}
	}
	if _, err := c.Writer.WriteString("data: "); err != nil {
		return false
	}
	if _, err := c.Writer.Write(data); err != nil {
		return false
	}
	if _, err := c.Writer.WriteString("\n\n"); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

func writeImageStreamDone(c *gin.Context) bool {
	if _, err := c.Writer.WriteString("data: [DONE]\n\n"); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

func imageGenerationProgress(c *gin.Context, index, total int, model string) func(int, time.Duration) {
	return func(attempt int, elapsed time.Duration) {
		// A heartbeat every ten seconds keeps SSE clients and reverse proxies
		// informed without flooding the stream during a normal image render.
		if attempt == 0 || attempt%5 != 0 {
			return
		}
		writeImageStreamEvent(c, "image.generation.progress", imageStreamChunk{
			Object:       "image.generation.progress",
			Index:        index,
			Total:        total,
			Model:        model,
			ProgressText: fmt.Sprintf("Image generation is still running (%ds)", int(elapsed.Seconds())),
		})
	}
}

func (h *ImageHandler) recordImageGenerationFailure(c *gin.Context, account *accounts.Account, err error) {
	if errors.Is(err, chatgpt.ErrImageGenerationTimeout) {
		rememberRequestFailure(c, "image_generation_timeout")
		return
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unauthorized") || strings.Contains(message, " 401") || strings.Contains(message, "status 401") {
		h.accountPool.ReportFailure(account)
		rememberRequestFailure(c, "image_account_auth_failed")
		return
	}
	rememberRequestFailure(c, "image_generation_failed")
}

// imageGenerationFailureResponse prevents internal ChatGPT schema dumps from
// being shown to OpenAI-compatible clients. Those dumps can be thousands of
// characters long and do not help a caller recover; the response still keeps
// a stable machine-readable code for retries or account remediation.
func imageGenerationFailureResponse(err error) (status int, message, code string) {
	if errors.Is(err, chatgpt.ErrImageGenerationTimeout) {
		return http.StatusGatewayTimeout, "Image generation timed out while waiting for the upstream result. Please retry.", "image_generation_timeout"
	}

	raw := strings.ToLower(err.Error())
	switch {
	case strings.Contains(raw, "status 422"):
		return http.StatusBadGateway, "The upstream service rejected the uploaded image attachment. Please retry with PNG or JPEG.", "upstream_image_attachment_rejected"
	case strings.Contains(raw, "content_policy") || strings.Contains(raw, "content policy"):
		return http.StatusBadRequest, "The image request was rejected by the upstream content policy.", "image_content_policy_violation"
	case strings.Contains(raw, "status 429") || strings.Contains(raw, "rate limit"):
		return http.StatusTooManyRequests, "The upstream image service is busy. Please retry shortly.", "upstream_rate_limited"
	case strings.Contains(raw, "status 401") || strings.Contains(raw, "status 403") || strings.Contains(raw, "unauthorized"):
		return http.StatusServiceUnavailable, "The selected image account is no longer authorized. Please switch accounts or refresh it.", "image_account_unavailable"
	default:
		return http.StatusBadGateway, "The upstream image service failed to complete the request. Please retry.", "upstream_image_generation_failed"
	}
}

// requestStreamFlag 解析 stream 参数,支持 JSON body 的 stream 字段或 ?stream=true 查询参数。
func requestStreamFlag(c *gin.Context, jsonStream bool) bool {
	if jsonStream {
		return true
	}
	if v := strings.ToLower(strings.TrimSpace(c.Query("stream"))); v == "true" || v == "1" || v == "yes" {
		return true
	}
	if v := strings.ToLower(strings.TrimSpace(c.PostForm("stream"))); v == "true" || v == "1" || v == "yes" {
		return true
	}
	return false
}

// isStreamTrue 把任意形式的 stream 字段值转换为 bool。
func isStreamTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// imagePromptWithPreferences turns OpenAI image options into an explicit
// upstream instruction. ChatGPT's web conversation endpoint has no native
// size/quality fields, so this is intentionally best-effort rather than a
// claim that the dimensions can be enforced server-side.
func imagePromptWithPreferences(prompt, size, quality string) (string, error) {
	size = strings.ToLower(strings.TrimSpace(size))
	quality = strings.ToLower(strings.TrimSpace(quality))
	if size != "" && size != "auto" {
		switch size {
		case "256x256", "512x512", "1024x1024", "1536x1024", "1024x1536", "1792x1024", "1024x1792":
		default:
			return "", fmt.Errorf("unsupported image size: %s", size)
		}
	}
	if quality != "" && quality != "auto" {
		switch quality {
		case "standard", "hd", "low", "medium", "high":
		default:
			return "", fmt.Errorf("unsupported image quality: %s", quality)
		}
	}
	if size == "" && quality == "" {
		return prompt, nil
	}
	var preferences []string
	if size != "" && size != "auto" {
		preferences = append(preferences, "requested canvas size: "+size)
	}
	if quality != "" && quality != "auto" {
		preferences = append(preferences, "requested image quality: "+quality)
	}
	return prompt + "\n\n[Rendering preferences: " + strings.Join(preferences, "; ") + ". Follow these when the image model supports them.]", nil
}

// ─── /v1/images/generations ──────────────────────────────────────

func (h *ImageHandler) Generations(c *gin.Context) {
	var imageRequest officialtypes.ImageGenerationRequest
	err := c.BindJSON(&imageRequest)
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Request must be proper JSON",
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    err.Error(),
		}})
		return
	}
	if imageRequest.Prompt == "" {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Missing required parameter: prompt",
			"type":    "invalid_request_error",
			"param":   "prompt",
			"code":    "missing_required_parameter",
		}})
		return
	}
	if imageRequest.N <= 0 {
		imageRequest.N = 1
	}
	if imageRequest.N > 10 {
		imageRequest.N = 10
	}
	upstreamPrompt, err := imagePromptWithPreferences(imageRequest.Prompt, imageRequest.Size, imageRequest.Quality)
	if err != nil {
		param := "size"
		if strings.Contains(err.Error(), "quality") {
			param = "quality"
		}
		c.JSON(400, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "invalid_request_error",
			"param":   param,
			"code":    "invalid_image_parameter",
		}})
		return
	}
	if imageRequest.ResponseFormat == "" {
		imageRequest.ResponseFormat = "b64_json"
	}

	account, _, err := resolveAccount(c, h.accountPool, h.cfg, true)
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "authorization_error",
			"param":   "Authorization",
			"code":    400,
		}})
		return
	}
	if account == nil || account.Token == "" {
		c.JSON(400, gin.H{"error": "Images API requires a logged-in ChatGPT access token."})
		c.Abort()
		return
	}
	if !account.Type.Satisfies(accounts.CapImageGenerate) {
		c.JSON(403, gin.H{"error": "Images API requires a logged-in ChatGPT account."})
		return
	}

	proxyUrl := account.Proxy
	client := setupImageClientWithProxy(proxyUrl)
	client.SetCookies("https://chatgpt.com", chatgpt.BasicCookies)
	turnStile, status, err := chatgpt.InitSentinel(client, account, proxyUrl, 0)
	if err != nil {
		if status == http.StatusUnauthorized {
			h.accountPool.ReportFailure(account)
		}
		c.JSON(status, gin.H{
			"message": err.Error(),
			"type":    "InitTurnStile_request_error",
			"param":   err,
			"code":    status,
		})
		return
	}

	stream := requestStreamFlag(c, imageRequest.Stream)
	if stream {
		writeImageStreamHeader(c)
	}

	var data []officialtypes.ImageGenerationData
	for i := 0; i < imageRequest.N; i++ {
		if stream {
			writeImageStreamEvent(c, "image.generation.chunk", imageStreamChunk{
				Object:       "image.generation.chunk",
				Index:        i,
				Total:        imageRequest.N,
				Created:      0,
				Model:        imageRequest.Model,
				ProgressText: fmt.Sprintf("Generating image %d/%d ...", i+1, imageRequest.N),
			})
		}
		var progress func(int, time.Duration)
		if stream {
			progress = imageGenerationProgress(c, i, imageRequest.N, imageRequest.Model)
		}
		imageResults, upstreamText, err := chatgpt.GeneratePictureConversationImagesWithProgress(client, account, turnStile, upstreamPrompt, imageRequest.Model, proxyUrl, progress)
		if err != nil {
			h.recordImageGenerationFailure(c, account, err)
			status, message, code := imageGenerationFailureResponse(err)
			if stream {
				writeImageStreamEvent(c, "image.generation.error", gin.H{
					"object":  "image.generation.error",
					"index":   i,
					"total":   imageRequest.N,
					"message": message,
					"code":    code,
				})
				writeImageStreamDone(c)
				return
			}
			c.JSON(status, gin.H{"error": gin.H{
				"message": message,
				"type":    "image_generation_error",
				"param":   nil,
				"code":    code,
			}})
			return
		}
		for _, imageResult := range imageResults {
			item := officialtypes.ImageGenerationData{
				RevisedPrompt: imageRequest.Prompt,
			}
			if imageRequest.ResponseFormat == "b64_json" {
				if imageResult.B64JSON != "" {
					item.B64JSON = imageResult.B64JSON
				} else if imageResult.URL != "" {
					imageBytes, err := chatgpt.DownloadImageBytes(client, imageResult.URL, account)
					if err != nil {
						if stream {
							writeImageStreamEvent(c, "image.generation.error", gin.H{
								"object":  "image.generation.error",
								"index":   i,
								"total":   imageRequest.N,
								"message": err.Error(),
							})
							writeImageStreamDone(c)
							return
						}
						c.JSON(500, gin.H{"error": gin.H{
							"message": err.Error(),
							"type":    "image_download_error",
							"param":   nil,
							"code":    "image_download_error",
						}})
						return
					}
					item.B64JSON = base64.StdEncoding.EncodeToString(imageBytes)
				}
			} else {
				item.URL = imageResult.URL
				if item.URL == "" && imageResult.B64JSON != "" {
					item.B64JSON = imageResult.B64JSON
				}
			}
			data = append(data, item)
			if stream {
				writeImageStreamEvent(c, "image.generation.result", imageStreamResult{
					Object:  "image.generation.result",
					Index:   len(data) - 1,
					Total:   imageRequest.N,
					Created: 0,
					Model:   imageRequest.Model,
					Data:    []officialtypes.ImageGenerationData{item},
				})
			}
			if len(data) >= imageRequest.N {
				break
			}
		}
		if len(imageResults) == 0 && upstreamText != "" {
			rememberRequestFailure(c, "image_generation_empty_result")
			if stream {
				writeImageStreamEvent(c, "image.generation.error", gin.H{
					"object":  "image.generation.error",
					"index":   i,
					"total":   imageRequest.N,
					"message": "No image result found in response: " + upstreamText,
				})
				writeImageStreamDone(c)
				return
			}
			c.JSON(500, gin.H{"error": gin.H{
				"message": "No image result found in response: " + upstreamText,
				"type":    "image_generation_error",
				"param":   nil,
				"code":    "image_generation_error",
			}})
			return
		}
		if len(data) >= imageRequest.N {
			break
		}
	}
	if len(data) == 0 {
		rememberRequestFailure(c, "image_generation_empty_result")
		if stream {
			writeImageStreamEvent(c, "image.generation.error", gin.H{
				"object":  "image.generation.error",
				"message": "No image result found in response",
			})
			writeImageStreamDone(c)
			return
		}
		c.JSON(500, gin.H{"error": gin.H{
			"message": "No image result found in response",
			"type":    "image_generation_error",
			"param":   nil,
			"code":    "image_generation_error",
		}})
		return
	}
	if stream {
		writeImageStreamEvent(c, "image.generation.completed", imageStreamCompleted{
			Object:  "image.generation.completed",
			Created: 0,
			Model:   imageRequest.Model,
			Data:    data,
		})
		writeImageStreamDone(c)
		return
	}
	c.JSON(200, officialtypes.NewImageGenerationResponse(data))
}

// ─── Image Edit / Variation types ────────────────────────────────

// editImageInput 一张待编辑/变体使用的源图,支持 multipart 文件上传与 JSON 引用。
type editImageInput struct {
	Data        []byte
	Filename    string
	ContentType string
}

// normalizeEditImageForUpload keeps source images predictable for the
// ChatGPT file service. WebP is valid input for many clients, but web API
// deployments do not consistently expose its dimensions or accept it as an
// image edit attachment. Converting it to PNG preserves pixels and gives the
// uploaded file unambiguous image metadata.
func normalizeEditImageForUpload(input editImageInput) (editImageInput, error) {
	if len(input.Data) == 0 {
		return editImageInput{}, fmt.Errorf("image data is empty")
	}
	if int64(len(input.Data)) > imageflow.MaxImageBytes {
		return editImageInput{}, fmt.Errorf("image exceeds the %d MiB limit", imageflow.MaxImageBytes>>20)
	}

	detectedType := http.DetectContentType(input.Data)
	declaredType := strings.ToLower(strings.TrimSpace(strings.Split(input.ContentType, ";")[0]))
	if declaredType == "" || declaredType == "application/octet-stream" {
		input.ContentType = detectedType
	} else {
		input.ContentType = declaredType
	}
	if input.Filename == "" {
		input.Filename = "image"
	}

	// Trust the file signature too: some clients label WebP as generic binary
	// data, while others preserve an incorrect original content type.
	if input.ContentType != "image/webp" && detectedType != "image/webp" {
		return input, nil
	}

	decoded, _, err := image.Decode(bytes.NewReader(input.Data))
	if err != nil {
		return editImageInput{}, fmt.Errorf("decode WebP image: %w", err)
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, decoded); err != nil {
		return editImageInput{}, fmt.Errorf("convert WebP image to PNG: %w", err)
	}
	if int64(pngData.Len()) > imageflow.MaxImageBytes {
		return editImageInput{}, fmt.Errorf("converted PNG exceeds the %d MiB limit", imageflow.MaxImageBytes>>20)
	}
	input.Data = pngData.Bytes()
	input.ContentType = "image/png"
	base := strings.TrimSuffix(input.Filename, filepath.Ext(input.Filename))
	if base == "" {
		base = "image"
	}
	input.Filename = base + ".png"
	return input, nil
}

// imageEditImageReferenceFields 与 chatgpt2api/api/image_inputs.IMAGE_REFERENCE_FIELDS 对齐。
var imageEditImageReferenceFields = map[string]bool{
	"image":       true,
	"image[]":     true,
	"images":      true,
	"images[]":    true,
	"image_url":   true,
	"image_url[]": true,
}

func normalizeImageEditImages(rawImages []interface{}) []editImageInput {
	out := make([]editImageInput, 0, len(rawImages))
	for _, raw := range rawImages {
		switch v := raw.(type) {
		case *multipart.FileHeader:
			if v == nil {
				continue
			}
			f, err := v.Open()
			if err != nil {
				continue
			}
			data, err := io.ReadAll(io.LimitReader(f, imageflow.MaxImageBytes+1))
			f.Close()
			if err != nil || len(data) == 0 || int64(len(data)) > imageflow.MaxImageBytes {
				continue
			}
			ct := v.Header.Get("Content-Type")
			if ct == "" {
				ct = "image/png"
			}
			name := v.Filename
			if name == "" {
				name = "image.png"
			}
			out = append(out, editImageInput{Data: data, Filename: name, ContentType: ct})
		case editImageInput:
			if len(v.Data) > 0 {
				out = append(out, v)
			}
		default:
			// JSON 形态的 image 引用由 collectImageEditSourcesFromValue 处理,这里跳过
		}
	}
	return out
}

func imageEditReadJSONImage(data []byte, filename, contentType string) (editImageInput, error) {
	if len(data) == 0 {
		return editImageInput{}, fmt.Errorf("image data is empty")
	}
	if int64(len(data)) > imageflow.MaxImageBytes {
		return editImageInput{}, fmt.Errorf("image exceeds the %d MiB limit", imageflow.MaxImageBytes>>20)
	}
	if filename == "" {
		filename = "image.png"
	}
	if contentType == "" {
		contentType = "image/png"
	}
	return editImageInput{Data: data, Filename: filename, ContentType: contentType}, nil
}

func imageEditDecodeBase64(value string) (editImageInput, error) {
	source, err := imageflow.DecodeBase64Image(value)
	if err != nil {
		return editImageInput{}, err
	}
	return editImageInput{Data: source.Data, Filename: source.Filename, ContentType: source.ContentType}, nil
}

func imageEditConvertURL(client httpclient.AuroraHttpClient, raw string) (editImageInput, bool, error) {
	item, ok, err := imageflow.NormalizeImageURL(client, raw)
	if err != nil || !ok {
		return editImageInput{}, ok, err
	}
	return editImageInput{Data: item.Data, Filename: item.Filename, ContentType: item.ContentType}, true, nil
}

func resolveEditImageSources(c *gin.Context, body map[string]interface{}, client httpclient.AuroraHttpClient) ([]editImageInput, error) {
	out := make([]editImageInput, 0, 2)
	appendValue := func(v interface{}) error {
		switch t := v.(type) {
		case string:
			item, ok, err := imageEditConvertURL(client, t)
			if err != nil {
				return err
			}
			if ok {
				out = append(out, item)
			}
		case map[string]interface{}:
			if urlVal, ok := t["image_url"]; ok {
				if s, ok := urlVal.(string); ok {
					item, _, err := imageEditConvertURL(client, s)
					if err != nil {
						return err
					}
					out = append(out, item)
				}
			} else if u, ok := t["url"]; ok {
				if s, ok := u.(string); ok {
					item, _, err := imageEditConvertURL(client, s)
					if err != nil {
						return err
					}
					out = append(out, item)
				}
			} else if b64, ok := t["b64_json"].(string); ok && b64 != "" {
				item, err := imageEditDecodeBase64(b64)
				if err != nil {
					return err
				}
				out = append(out, item)
			} else if b64, ok := t["base64"].(string); ok && b64 != "" {
				item, err := imageEditDecodeBase64(b64)
				if err != nil {
					return err
				}
				out = append(out, item)
			}
		}
		return nil
	}

	for _, key := range []string{"images", "image", "image_url"} {
		val, ok := body[key]
		if !ok || val == nil {
			continue
		}
		switch arr := val.(type) {
		case []interface{}:
			for _, item := range arr {
				if err := appendValue(item); err != nil {
					return nil, err
				}
			}
		case string:
			if err := appendValue(arr); err != nil {
				return nil, err
			}
		case map[string]interface{}:
			if err := appendValue(arr); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// collectResponsesAPIParts 从 Responses API 风格的 input / content / messages 提取文本和图片。
func collectResponsesAPIParts(raw interface{}) (string, []string) {
	if raw == nil {
		return "", nil
	}
	var textParts []string
	var imageURLs []string

	appendFromContent := func(content interface{}) {
		switch c := content.(type) {
		case string:
			if strings.TrimSpace(c) != "" {
				textParts = append(textParts, c)
			}
		case []interface{}:
			for _, item := range c {
				part, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				partType := strings.ToLower(strings.TrimSpace(stringFromAny(part["type"])))
				switch partType {
				case "input_text", "text", "output_text":
					if s := stringFromAny(part["text"]); s != "" {
						textParts = append(textParts, s)
					}
				case "input_image", "image", "image_url":
					switch u := part["image_url"].(type) {
					case string:
						imageURLs = append(imageURLs, u)
					case map[string]interface{}:
						if s := stringFromAny(u["url"]); s != "" {
							imageURLs = append(imageURLs, s)
						}
					}
					if s := stringFromAny(part["url"]); s != "" {
						imageURLs = append(imageURLs, s)
					}
				}
			}
		}
	}

	switch v := raw.(type) {
	case string:
		textParts = append(textParts, v)
	case map[string]interface{}:
		appendFromContent(v["content"])
	case []interface{}:
		for _, item := range v {
			switch m := item.(type) {
			case string:
				textParts = append(textParts, m)
			case map[string]interface{}:
				partType := strings.ToLower(strings.TrimSpace(stringFromAny(m["type"])))
				if partType == "input_image" || partType == "image" || partType == "image_url" {
					switch u := m["image_url"].(type) {
					case string:
						imageURLs = append(imageURLs, u)
					case map[string]interface{}:
						if s := stringFromAny(u["url"]); s != "" {
							imageURLs = append(imageURLs, s)
						}
					}
					if s := stringFromAny(m["url"]); s != "" {
						imageURLs = append(imageURLs, s)
					}
					continue
				}
				appendFromContent(m["content"])
			}
		}
	}
	return strings.Join(textParts, "\n"), imageURLs
}

func stringFromAny(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ─── /v1/images/edits 与 /v1/images/variations ───────────────────

func (h *ImageHandler) Edits(c *gin.Context) {
	h.runImageEditFlow(c, false)
}

func (h *ImageHandler) Variations(c *gin.Context) {
	h.runImageEditFlow(c, true)
}

func (h *ImageHandler) runImageEditFlow(c *gin.Context, asVariation bool) {
	contentType := strings.Split(c.GetHeader("Content-Type"), ";")[0]
	contentType = strings.ToLower(strings.TrimSpace(contentType))

	var imageSources []editImageInput
	var prompt, model, responseFormat, size string
	var n int
	var stream bool

	parseFormFields := func(promptVal, modelVal, nVal, sizeVal, responseFormatVal, streamVal string) {
		prompt = strings.TrimSpace(promptVal)
		model = strings.TrimSpace(modelVal)
		size = strings.TrimSpace(sizeVal)
		responseFormat = strings.TrimSpace(responseFormatVal)
		if nVal != "" {
			if v, err := strconv.Atoi(nVal); err == nil {
				n = v
			}
		}
		stream = isStreamTrue(streamVal)
	}

	if contentType == "application/json" {
		var body struct {
			Prompt         string                   `json:"prompt"`
			Model          string                   `json:"model"`
			N              int                      `json:"n"`
			Size           string                   `json:"size"`
			ResponseFormat string                   `json:"response_format"`
			Stream         bool                     `json:"stream"`
			Images         []map[string]interface{} `json:"images,omitempty"`
			Image          map[string]interface{}   `json:"image,omitempty"`
			ImageURL       interface{}              `json:"image_url,omitempty"`
			Input          interface{}              `json:"input,omitempty"`
			Content        interface{}              `json:"content,omitempty"`
			Messages       interface{}              `json:"messages,omitempty"`
		}
		if err := c.BindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": gin.H{
				"message": "Request must be proper JSON",
				"type":    "invalid_request_error",
				"param":   nil,
				"code":    err.Error(),
			}})
			return
		}
		prompt = strings.TrimSpace(body.Prompt)
		model = body.Model
		size = body.Size
		n = body.N
		responseFormat = body.ResponseFormat
		stream = body.Stream

		client := bogdanfinn.NewStdClient()

		appendJSONSource := func(src map[string]interface{}) error {
			var item editImageInput
			var err error
			switch {
			case stringFromAny(src["image_url"]) != "":
				item, _, err = imageEditConvertURL(client, stringFromAny(src["image_url"]))
			case stringFromAny(src["url"]) != "":
				item, _, err = imageEditConvertURL(client, stringFromAny(src["url"]))
			case stringFromAny(src["b64_json"]) != "":
				item, err = imageEditDecodeBase64(stringFromAny(src["b64_json"]))
			case stringFromAny(src["base64"]) != "":
				item, err = imageEditDecodeBase64(stringFromAny(src["base64"]))
			default:
				return fmt.Errorf("missing image_url, b64_json, or base64")
			}
			if err != nil {
				return err
			}
			if len(item.Data) > 0 {
				imageSources = append(imageSources, item)
			}
			return nil
		}
		for index, src := range body.Images {
			if err := appendJSONSource(src); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
					"message": "invalid image reference: " + err.Error(),
					"type":    "invalid_request_error",
					"param":   fmt.Sprintf("images[%d]", index),
					"code":    "invalid_image",
				}})
				return
			}
		}
		if body.Image != nil {
			if err := appendJSONSource(body.Image); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
					"message": "invalid image reference: " + err.Error(),
					"type":    "invalid_request_error",
					"param":   "image",
					"code":    "invalid_image",
				}})
				return
			}
		}
		if body.ImageURL != nil {
			var appendImageURLValue func(interface{}) error
			appendImageURLValue = func(value interface{}) error {
				switch t := value.(type) {
				case string:
					item, _, err := imageEditConvertURL(client, t)
					if err != nil {
						return err
					}
					if len(item.Data) > 0 {
						imageSources = append(imageSources, item)
					}
				case map[string]interface{}:
					if err := appendJSONSource(t); err != nil {
						return err
					}
				case []interface{}:
					for _, item := range t {
						if err := appendImageURLValue(item); err != nil {
							return err
						}
					}
				default:
					return fmt.Errorf("image_url must be a string, object, or array")
				}
				return nil
			}
			if err := appendImageURLValue(body.ImageURL); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid image reference: " + err.Error(), "type": "invalid_request_error", "param": "image_url", "code": "invalid_image"}})
				return
			}
		}

		promptFromParts, imageParts := collectResponsesAPIParts(body.Input)
		if len(imageParts) == 0 {
			if p, imgs := collectResponsesAPIParts(body.Content); len(imgs) > 0 {
				promptFromParts = p
				imageParts = imgs
			}
		}
		if len(imageParts) == 0 {
			if p, imgs := collectResponsesAPIParts(body.Messages); len(imgs) > 0 {
				promptFromParts = p
				imageParts = imgs
			}
		}
		for _, p := range imageParts {
			item, _, err := imageEditConvertURL(client, p)
			if err != nil {
				c.JSON(400, gin.H{"error": gin.H{
					"message": "invalid image reference: " + err.Error(),
					"type":    "invalid_request_error",
					"param":   "input",
					"code":    "invalid_image",
				}})
				return
			}
			if len(item.Data) > 0 {
				imageSources = append(imageSources, item)
			}
		}
		if prompt == "" {
			prompt = strings.TrimSpace(promptFromParts)
		}
	} else {
		form, err := c.MultipartForm()
		if err != nil {
			c.JSON(400, gin.H{"error": gin.H{
				"message": "Request must be multipart/form-data or application/json: " + err.Error(),
				"type":    "invalid_request_error",
				"param":   nil,
				"code":    "invalid_multipart",
			}})
			return
		}
		parseFormFields(
			strings.TrimSpace(c.PostForm("prompt")),
			strings.TrimSpace(c.PostForm("model")),
			c.PostForm("n"),
			strings.TrimSpace(c.PostForm("size")),
			strings.TrimSpace(c.PostForm("response_format")),
			c.PostForm("stream"),
		)
		rawSources := make([]interface{}, 0, 4)
		for _, key := range []string{"image", "image[]", "images", "images[]"} {
			if vs, ok := form.File[key]; ok {
				for _, fh := range vs {
					rawSources = append(rawSources, fh)
				}
			}
		}
		if vs, ok := form.Value["image_url"]; ok {
			client := bogdanfinn.NewStdClient()
			for index, s := range vs {
				item, _, err := imageEditConvertURL(client, s)
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
						"message": "invalid image reference: " + err.Error(),
						"type":    "invalid_request_error",
						"param":   fmt.Sprintf("image_url[%d]", index),
						"code":    "invalid_image",
					}})
					return
				}
				if len(item.Data) > 0 {
					imageSources = append(imageSources, item)
				}
			}
		}
		imageSources = append(imageSources, normalizeImageEditImages(rawSources)...)
	}

	if asVariation {
		if prompt == "" {
			prompt = "Generate a variation of the provided image(s). Return only the generated image, not a text description."
		}
	}

	if len(imageSources) == 0 {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Missing required image input. Provide multipart 'image'/'images' field, or JSON 'image'/'images' field with image_url.",
			"type":    "invalid_request_error",
			"param":   "image",
			"code":    "missing_required_parameter",
		}})
		return
	}
	if err := validateImageEditSources(imageSources); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "invalid_request_error",
			"param":   "images",
			"code":    "invalid_image",
		}})
		return
	}
	if !asVariation && prompt == "" {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Missing required parameter: prompt",
			"type":    "invalid_request_error",
			"param":   "prompt",
			"code":    "missing_required_parameter",
		}})
		return
	}
	for i, source := range imageSources {
		normalized, err := normalizeEditImageForUpload(source)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"message": "invalid image input: " + err.Error(),
				"type":    "invalid_request_error",
				"param":   fmt.Sprintf("image[%d]", i),
				"code":    "invalid_image",
			}})
			return
		}
		imageSources[i] = normalized
	}
	if err := validateImageEditSources(imageSources); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "invalid_request_error",
			"param":   "images",
			"code":    "invalid_image",
		}})
		return
	}
	upstreamPrompt, err := imagePromptWithPreferences(prompt, size, "")
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "invalid_request_error",
			"param":   "size",
			"code":    "invalid_image_parameter",
		}})
		return
	}
	if n <= 0 {
		n = 1
	}
	if n > 10 {
		n = 10
	}
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream = requestStreamFlag(c, stream)

	account, _, err := resolveAccount(c, h.accountPool, h.cfg, true)
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "authorization_error",
			"param":   "Authorization",
			"code":    400,
		}})
		return
	}
	if account == nil || account.Token == "" {
		c.JSON(400, gin.H{"error": "Images API requires a logged-in ChatGPT access token."})
		c.Abort()
		return
	}
	if !account.Type.Satisfies(accounts.CapImageGenerate) {
		c.JSON(403, gin.H{"error": "Images API requires a logged-in ChatGPT account."})
		return
	}
	if asVariation {
		if !account.Type.Satisfies(accounts.CapImageVariation) {
			c.JSON(403, gin.H{"error": "Image variation requires a logged-in ChatGPT account."})
			return
		}
	} else {
		if !account.Type.Satisfies(accounts.CapImageEdit) {
			c.JSON(403, gin.H{"error": "Image edit requires a logged-in ChatGPT account."})
			return
		}
	}

	proxyUrl := account.Proxy
	client := setupImageClientWithProxy(proxyUrl)
	client.SetCookies("https://chatgpt.com", chatgpt.BasicCookies)
	turnStile, status, err := chatgpt.InitSentinel(client, account, proxyUrl, 0)
	if err != nil {
		if status == http.StatusUnauthorized {
			h.accountPool.ReportFailure(account)
		}
		c.JSON(status, gin.H{
			"message": err.Error(),
			"type":    "InitTurnStile_request_error",
			"param":   err,
			"code":    status,
		})
		return
	}

	if stream {
		writeImageStreamHeader(c)
	}

	// 1) 上传所有源图
	references := make([]chatgpt.ImageEditReference, 0, len(imageSources))
	for idx, src := range imageSources {
		uploaded, upStatus, upErr := uploadImageWithRetry(client, account, proxyUrl, src.Filename, src.ContentType, src.Data)
		if upErr != nil {
			message := fmt.Sprintf("upload image %d/%d failed: %s", idx+1, len(imageSources), upErr.Error())
			if stream {
				writeImageStreamEvent(c, "image.generation.error", gin.H{
					"object":  "image.generation.error",
					"index":   idx,
					"message": message,
				})
				writeImageStreamDone(c)
				return
			}
			c.JSON(upStatus, gin.H{"error": gin.H{
				"message": message,
				"type":    "image_upload_error",
				"param":   fmt.Sprintf("image[%d]", idx),
				"code":    "image_upload_error",
			}})
			return
		}
		references = append(references, chatgpt.ImageEditReference{
			FileID:        uploaded.FileID,
			LibraryFileID: uploaded.LibraryFileID,
			Width:         uploaded.Width,
			Height:        uploaded.Height,
			Size:          int(uploaded.Bytes),
			MimeType:      uploaded.MimeType,
			Filename:      uploaded.Filename,
		})
	}

	// 2) 调起带 reference 的 image conversation,循环 n 次以满足 N
	var data []officialtypes.ImageGenerationData
	for i := 0; i < n; i++ {
		if stream {
			writeImageStreamEvent(c, "image.generation.chunk", imageStreamChunk{
				Object:       "image.generation.chunk",
				Index:        i,
				Total:        n,
				Created:      0,
				Model:        model,
				ProgressText: fmt.Sprintf("Generating image %d/%d ...", i+1, n),
			})
		}
		var progress func(int, time.Duration)
		if stream {
			progress = imageGenerationProgress(c, i, n, model)
		}
		imageResults, upstreamText, err := chatgpt.GeneratePictureConversationImagesWithReferencesProgress(client, account, turnStile, upstreamPrompt, model, proxyUrl, references, progress)
		if err != nil {
			h.recordImageGenerationFailure(c, account, err)
			status, message, code := imageGenerationFailureResponse(err)
			if stream {
				writeImageStreamEvent(c, "image.generation.error", gin.H{
					"object":  "image.generation.error",
					"index":   i,
					"total":   n,
					"message": message,
					"code":    code,
				})
				writeImageStreamDone(c)
				return
			}
			c.JSON(status, gin.H{"error": gin.H{
				"message": message,
				"type":    "image_generation_error",
				"param":   nil,
				"code":    code,
			}})
			return
		}
		for _, imageResult := range imageResults {
			item := officialtypes.ImageGenerationData{
				RevisedPrompt: prompt,
			}
			if responseFormat == "b64_json" {
				if imageResult.B64JSON != "" {
					item.B64JSON = imageResult.B64JSON
				} else if imageResult.URL != "" {
					imageBytes, err := chatgpt.DownloadImageBytes(client, imageResult.URL, account)
					if err != nil {
						if stream {
							writeImageStreamEvent(c, "image.generation.error", gin.H{
								"object":  "image.generation.error",
								"index":   i,
								"total":   n,
								"message": err.Error(),
							})
							writeImageStreamDone(c)
							return
						}
						c.JSON(500, gin.H{"error": gin.H{
							"message": err.Error(),
							"type":    "image_download_error",
							"param":   nil,
							"code":    "image_download_error",
						}})
						return
					}
					item.B64JSON = base64.StdEncoding.EncodeToString(imageBytes)
				}
			} else {
				item.URL = imageResult.URL
				if item.URL == "" && imageResult.B64JSON != "" {
					item.B64JSON = imageResult.B64JSON
				}
			}
			data = append(data, item)
			if stream {
				writeImageStreamEvent(c, "image.generation.result", imageStreamResult{
					Object:  "image.generation.result",
					Index:   len(data) - 1,
					Total:   n,
					Created: 0,
					Model:   model,
					Data:    []officialtypes.ImageGenerationData{item},
				})
			}
			if len(data) >= n {
				break
			}
		}
		if len(imageResults) == 0 && upstreamText != "" {
			rememberRequestFailure(c, "image_generation_empty_result")
			if stream {
				writeImageStreamEvent(c, "image.generation.error", gin.H{
					"object":  "image.generation.error",
					"index":   i,
					"total":   n,
					"message": "No image result found in response: " + upstreamText,
				})
				writeImageStreamDone(c)
				return
			}
			c.JSON(500, gin.H{"error": gin.H{
				"message": "No image result found in response: " + upstreamText,
				"type":    "image_generation_error",
				"param":   nil,
				"code":    "image_generation_error",
			}})
			return
		}
		if len(data) >= n {
			break
		}
	}
	if len(data) == 0 {
		rememberRequestFailure(c, "image_generation_empty_result")
		if stream {
			writeImageStreamEvent(c, "image.generation.error", gin.H{
				"object":  "image.generation.error",
				"message": "No image result found in response",
			})
			writeImageStreamDone(c)
			return
		}
		c.JSON(500, gin.H{"error": gin.H{
			"message": "No image result found in response",
			"type":    "image_generation_error",
			"param":   nil,
			"code":    "image_generation_error",
		}})
		return
	}
	if stream {
		writeImageStreamEvent(c, "image.generation.completed", imageStreamCompleted{
			Object:  "image.generation.completed",
			Created: 0,
			Model:   model,
			Data:    data,
		})
		writeImageStreamDone(c)
		return
	}
	if asVariation {
		c.JSON(200, officialtypes.NewImageVariationResponse(data))
	} else {
		c.JSON(200, officialtypes.NewImageEditResponse(data))
	}
}

func validateImageEditSources(sources []editImageInput) error {
	if len(sources) > maxImageEditSources {
		return fmt.Errorf("a maximum of %d source images is supported per request", maxImageEditSources)
	}
	var totalBytes int64
	for _, source := range sources {
		totalBytes += int64(len(source.Data))
		if totalBytes > maxImageEditTotalBytes {
			return fmt.Errorf("combined image size exceeds the %d MiB limit", maxImageEditTotalBytes>>20)
		}
	}
	return nil
}

func uploadImageWithRetry(client httpclient.AuroraHttpClient, account *accounts.Account, proxyURL, filename, contentType string, data []byte) (chatgpt.UploadedFile, int, error) {
	var uploaded chatgpt.UploadedFile
	var status int
	var err error
	for attempt := 1; attempt <= imageUploadMaxAttempts; attempt++ {
		uploaded, status, err = chatgpt.UploadFile(client, account, proxyURL, filename, contentType, data)
		if err == nil || !isRetryableImageUploadStatus(status) || attempt == imageUploadMaxAttempts {
			return uploaded, status, err
		}
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	return uploaded, status, err
}

func isRetryableImageUploadStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}
