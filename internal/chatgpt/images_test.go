package chatgpt

import "testing"

func TestBuildImageEditMessageContentIncludesCompleteAttachmentMetadata(t *testing.T) {
	content, metadata := buildImageEditMessageContent([]ImageEditReference{{
		FileID:        "file-123",
		LibraryFileID: "library-456",
		Filename:      "source.png",
		MimeType:      "image/png",
		Size:          1024,
		Width:         32,
		Height:        16,
	}}, "make the sky blue")

	if content["content_type"] != "multimodal_text" {
		t.Fatalf("content_type = %#v, want multimodal_text", content["content_type"])
	}
	parts, ok := content["parts"].([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("parts = %#v, want image pointer and prompt", content["parts"])
	}
	pointer, ok := parts[0].(map[string]interface{})
	if !ok || pointer["content_type"] != "image_asset_pointer" || pointer["asset_pointer"] != "file-service://file-123" {
		t.Fatalf("image pointer = %#v", parts[0])
	}
	attachments, ok := metadata["attachments"].([]map[string]interface{})
	if !ok || len(attachments) != 1 {
		t.Fatalf("attachments = %#v", metadata["attachments"])
	}
	attachment := attachments[0]
	for key, want := range map[string]interface{}{
		"id":              "file-123",
		"library_file_id": "library-456",
		"mime_type":       "image/png",
		"mimeType":        "image/png",
		"source":          "library",
		"is_big_paste":    false,
	} {
		if got := attachment[key]; got != want {
			t.Errorf("attachment[%q] = %#v, want %#v", key, got, want)
		}
	}
	if _, ok := metadata["selected_sources"]; !ok {
		t.Fatal("metadata is missing selected_sources")
	}
}
