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
