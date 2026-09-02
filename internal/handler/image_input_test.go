package handler

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"aurora/internal/chatgpt"
)

func TestImageEditDecodeBase64(t *testing.T) {
	raw := []byte("\x89PNG\r\n\x1a\n")
	input, err := imageEditDecodeBase64(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("imageEditDecodeBase64 returned error: %v", err)
	}
	if input.ContentType != "image/png" || !bytes.Equal(input.Data, raw) {
		t.Fatalf("input = %#v, want decoded PNG", input)
	}
}

func TestValidateImageEditSources(t *testing.T) {
	tooMany := make([]editImageInput, maxImageEditSources+1)
	if err := validateImageEditSources(tooMany); err == nil {
		t.Fatal("validateImageEditSources accepted too many images")
	}
	tooLarge := []editImageInput{{Data: make([]byte, maxImageEditTotalBytes+1)}}
	if err := validateImageEditSources(tooLarge); err == nil {
		t.Fatal("validateImageEditSources accepted oversized aggregate")
	}
}

func TestIsRetryableImageUploadStatus(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		if !isRetryableImageUploadStatus(status) {
			t.Errorf("status %d should be retryable", status)
		}
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusUnprocessableEntity} {
		if isRetryableImageUploadStatus(status) {
			t.Errorf("status %d should not be retryable", status)
		}
	}
}

func TestNormalizeEditImageForUploadKeepsPNG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var raw bytes.Buffer
	if err := png.Encode(&raw, img); err != nil {
		t.Fatal(err)
	}

	normalized, err := normalizeEditImageForUpload(editImageInput{
		Data:        raw.Bytes(),
		Filename:    "source.png",
		ContentType: "image/png",
	})
	if err != nil {
		t.Fatalf("normalizeEditImageForUpload returned error: %v", err)
	}
	if normalized.ContentType != "image/png" || normalized.Filename != "source.png" {
		t.Fatalf("normalized = %#v, want PNG metadata", normalized)
	}
	if !bytes.Equal(normalized.Data, raw.Bytes()) {
		t.Fatal("PNG input should not be re-encoded")
	}
}

func TestImageGenerationFailureResponseSanitizesUpstreamSchemaDump(t *testing.T) {
	status, message, code := imageGenerationFailureResponse(errors.New("image conversation failed (status 422): expected midi_asset_pointer, url required"))
	if status != http.StatusBadGateway || code != "upstream_image_attachment_rejected" {
		t.Fatalf("response = (%d, %q, %q)", status, message, code)
	}
	if strings.Contains(message, "midi_asset_pointer") || strings.Contains(message, "url required") {
		t.Fatalf("message leaked upstream schema dump: %q", message)
	}

	status, _, code = imageGenerationFailureResponse(chatgpt.ErrImageGenerationTimeout)
	if status != http.StatusGatewayTimeout || code != "image_generation_timeout" {
		t.Fatalf("timeout response = (%d, %q)", status, code)
	}
}

func TestNormalizeEditImageForUploadConvertsWebPToPNG(t *testing.T) {
	webp, err := base64.StdEncoding.DecodeString("UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA==")
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeEditImageForUpload(editImageInput{
		Data:        webp,
		Filename:    "source.webp",
		ContentType: "image/webp",
	})
	if err != nil {
		t.Fatalf("normalizeEditImageForUpload returned error: %v", err)
	}
	if normalized.ContentType != "image/png" || normalized.Filename != "source.png" {
		t.Fatalf("normalized metadata = %#v, want PNG", normalized)
	}
	if got := http.DetectContentType(normalized.Data); got != "image/png" {
		t.Fatalf("normalized data type = %q, want image/png", got)
	}
}
