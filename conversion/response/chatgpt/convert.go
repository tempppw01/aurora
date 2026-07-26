package chatgpt

import (
	"strings"

	"aurora/typings"
	chatgpt_types "aurora/typings/chatgpt"
	official_types "aurora/typings/official"
)

func ConvertToString(chatgpt_response *chatgpt_types.ChatGPTResponse, previous_text *typings.StringStruct, role bool, model string) string {
	currentText := TextFromParts(chatgpt_response.Message.Content.Parts)
	deltaText := SanitizedSnapshotDelta(previous_text.Text, currentText)
	previous_text.Text = currentText
	translated_response := official_types.NewChatCompletionChunk(deltaText, model)
	if role {
		translated_response.Choices[0].Delta.Role = chatgpt_response.Message.Author.Role
	} else if translated_response.Choices[0].Delta.Content == "" || translated_response.Choices[0].Delta.Content == "【" {
		return translated_response.Choices[0].Delta.Content
	}
	return "data: " + translated_response.String() + "\n\n"
}

// TextFromParts keeps all textual parts from a ChatGPT message. Rich replies
// can interleave image descriptors and text; taking only Parts[0] loses any
// text positioned after an image descriptor.
func TextFromParts(parts []interface{}) string {
	var text strings.Builder
	for _, part := range parts {
		appendTextPart(&text, part)
	}
	return text.String()
}

// appendTextPart accepts both the historical string parts and the structured
// text parts emitted by newer rich-response streams. Image descriptors are
// deliberately ignored so their prompts or asset IDs never become answer text.
func appendTextPart(text *strings.Builder, part interface{}) {
	switch value := part.(type) {
	case string:
		text.WriteString(value)
	case []interface{}:
		for _, nested := range value {
			appendTextPart(text, nested)
		}
	case map[string]interface{}:
		contentType, _ := value["content_type"].(string)
		if strings.Contains(strings.ToLower(contentType), "image") || value["asset_pointer"] != nil {
			return
		}
		for _, key := range []string{"text", "content", "value"} {
			if nested, ok := value[key]; ok {
				appendTextPart(text, nested)
				return
			}
		}
		if nested, ok := value["parts"]; ok {
			appendTextPart(text, nested)
		}
	}
}
