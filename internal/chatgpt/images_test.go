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

func TestBuildImageEditMessageContentKeepsAllReferencesInOrder(t *testing.T) {
	content, metadata := buildImageEditMessageContent([]ImageEditReference{
		{FileID: "file-first", Filename: "first.png", MimeType: "image/png"},
		{FileID: "file-second", Filename: "second.jpg", MimeType: "image/jpeg"},
	}, "combine both")

	parts := content["parts"].([]interface{})
	if len(parts) != 3 {
		t.Fatalf("parts length = %d, want 3", len(parts))
	}
	for index, want := range []string{"file-service://file-first", "file-service://file-second"} {
		part := parts[index].(map[string]interface{})
		if part["asset_pointer"] != want {
			t.Errorf("parts[%d].asset_pointer = %#v, want %q", index, part["asset_pointer"], want)
		}
	}
	attachments := metadata["attachments"].([]map[string]interface{})
	if len(attachments) != 2 || attachments[0]["id"] != "file-first" || attachments[1]["id"] != "file-second" {
		t.Fatalf("attachments = %#v, want both references in order", attachments)
	}
}

func TestCollectImageResultsFromConversationExcludesInputReferences(t *testing.T) {
	conversation := map[string]interface{}{
		"mapping": map[string]interface{}{
			"input": map[string]interface{}{
				"message": map[string]interface{}{
					"author": map[string]interface{}{"role": "user"},
					"content": map[string]interface{}{
						"content_type": "multimodal_text",
						"parts":        []interface{}{map[string]interface{}{"asset_pointer": "file-service://file-input123"}},
					},
				},
			},
			"output": map[string]interface{}{
				"message": map[string]interface{}{
					"author": map[string]interface{}{"role": "assistant"},
					"content": map[string]interface{}{
						"content_type": "multimodal_text",
						"parts":        []interface{}{map[string]interface{}{"url": "https://example.test/generated.png"}},
					},
				},
			},
		},
	}

	results := collectImageResultsFromConversation(nil, nil, conversation, map[string]bool{"file-input123": true})
	if len(results) != 1 || results[0].URL != "https://example.test/generated.png" {
		t.Fatalf("results = %#v, want only the generated image", results)
	}
}
