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
		if value, ok := part.(string); ok {
			text.WriteString(value)
		}
	}
	return text.String()
}
