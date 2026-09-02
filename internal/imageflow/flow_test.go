package imageflow

import (
	"bytes"
	"encoding/base64"
	"net"
	"testing"
)

func TestValidateRemoteImageURLRejectsPrivateAddresses(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/image.png",
		"http://10.0.0.1/image.png",
		"http://[::1]/image.png",
		"http://localhost/image.png",
	} {
		if _, err := validateRemoteImageURL(raw); err == nil {
			t.Errorf("validateRemoteImageURL(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestDecodeBase64Image(t *testing.T) {
	pngData := []byte("\x89PNG\r\n\x1a\n")
	source, err := DecodeBase64Image(base64.RawStdEncoding.EncodeToString(pngData))
	if err != nil {
		t.Fatalf("DecodeBase64Image returned error: %v", err)
	}
	if source.ContentType != "image/png" || source.Filename != "image.png" || !bytes.Equal(source.Data, pngData) {
		t.Fatalf("source = %#v, want decoded PNG", source)
	}
}

func TestDecodeBase64ImageRejectsInvalidData(t *testing.T) {
	if _, err := DecodeBase64Image("not base64"); err == nil {
		t.Fatal("DecodeBase64Image unexpectedly accepted invalid data")
	}
}

func TestIsPrivateAddress(t *testing.T) {
	if !isPrivateAddress(net.ParseIP("169.254.1.1")) {
		t.Fatal("link-local address must be rejected")
	}
	if isPrivateAddress(net.ParseIP("8.8.8.8")) {
		t.Fatal("public address must be allowed")
	}
}

func TestNormalizeImageURL_Empty(t *testing.T) {
	_, ok, err := NormalizeImageURL(nil, "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for empty string")
	}
}

func TestNormalizeImageURL_Whitespace(t *testing.T) {
	_, ok, err := NormalizeImageURL(nil, "   ")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for whitespace string")
	}
}

func TestDecodeDataURL_ValidPNG(t *testing.T) {
	// data:image/png;base64,iVBORw0KGgo=
	dataURL := "data:image/png;base64,iVBORw0KGgo="
	src, ok, err := decodeDataURL(dataURL)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected ok=true")
	}
	if src.ContentType != "image/png" {
		t.Errorf("expected content type image/png, got %s", src.ContentType)
	}
	if src.Filename != "image_url.png" {
		t.Errorf("expected filename image_url.png, got %s", src.Filename)
	}
}

func TestDecodeDataURL_Invalid(t *testing.T) {
	_, _, err := decodeDataURL("not-a-data-url")
	if err == nil {
		t.Error("expected error for invalid data URL")
	}
}

func TestNormalizeMultipartImages_Empty(t *testing.T) {
	result := NormalizeMultipartImages(nil)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d items", len(result))
	}
}

func TestCollectResponsesAPIParts_Nil(t *testing.T) {
	text, urls := CollectResponsesAPIParts(nil)
	if text != "" {
		t.Errorf("expected empty text, got %q", text)
	}
	if len(urls) != 0 {
		t.Errorf("expected empty urls, got %d items", len(urls))
	}
}

func TestCollectResponsesAPIParts_String(t *testing.T) {
	text, urls := CollectResponsesAPIParts("hello world")
	if text != "hello world" {
		t.Errorf("expected 'hello world', got %q", text)
	}
	if len(urls) != 0 {
		t.Errorf("expected empty urls, got %d items", len(urls))
	}
}

func TestCollectResponsesAPIParts_TextPart(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"type": "input_text",
			"text": "describe this image",
		},
	}
	text, urls := CollectResponsesAPIParts(input)
	if text != "describe this image" {
		t.Errorf("expected 'describe this image', got %q", text)
	}
	if len(urls) != 0 {
		t.Errorf("expected empty urls, got %d items", len(urls))
	}
}

func TestCollectResponsesAPIParts_ImagePart(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"type":      "input_image",
			"image_url": "https://example.com/image.png",
		},
	}
	text, urls := CollectResponsesAPIParts(input)
	if text != "" {
		t.Errorf("expected empty text, got %q", text)
	}
	if len(urls) != 1 || urls[0] != "https://example.com/image.png" {
		t.Errorf("expected one url, got %v", urls)
	}
}

func TestCollectResponsesAPIParts_MixedParts(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"type": "input_text",
			"text": "what is this?",
		},
		map[string]interface{}{
			"type":      "input_image",
			"image_url": "https://example.com/photo.jpg",
		},
	}
	text, urls := CollectResponsesAPIParts(input)
	if text != "what is this?" {
		t.Errorf("expected 'what is this?', got %q", text)
	}
	if len(urls) != 1 || urls[0] != "https://example.com/photo.jpg" {
		t.Errorf("expected one url, got %v", urls)
	}
}
