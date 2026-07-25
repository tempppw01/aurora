package chatgpt

import "strings"

// Internal citation markers are rendered by the ChatGPT web client, but are not
// part of the OpenAI-compatible response format. For example:
//
//	\uE200cite\uE202turn0search0\uE201
//
// Some upstream streams omit the closing marker. Those must be removed too,
// without swallowing the human-readable text that follows the turn id.
const (
	internalCitationOpen  = "\uE200cite\uE202"
	internalCitationClose = "\uE201"
)

// StripInternalCitationMarkers removes ChatGPT web-only citation control
// sequences while leaving the surrounding answer untouched.
func StripInternalCitationMarkers(text string) string {
	var result strings.Builder
	for {
		start := strings.Index(text, internalCitationOpen)
		if start < 0 {
			if partialStart := incompleteCitationStart(text); partialStart >= 0 {
				result.WriteString(text[:partialStart])
				return result.String()
			}
			result.WriteString(text)
			return result.String()
		}

		result.WriteString(text[:start])
		remainder := text[start+len(internalCitationOpen):]
		if end := strings.Index(remainder, internalCitationClose); end >= 0 {
			text = remainder[end+len(internalCitationClose):]
			continue
		}

		// A malformed marker commonly ends after "turn0" instead of the
		// closing control character. Remove its ASCII reference only, then
		// preserve all following natural-language content.
		if length := internalCitationReferenceLength(remainder); length > 0 {
			text = remainder[length:]
			continue
		}

		// The marker may be split across stream chunks. Do not leak a partial
		// control sequence; a later chunk will make the complete text visible.
		return result.String()
	}
}

func incompleteCitationStart(text string) int {
	for length := len(internalCitationOpen) - 1; length > 0; length-- {
		if strings.HasSuffix(text, internalCitationOpen[:length]) {
			return len(text) - length
		}
	}
	return -1
}

func internalCitationReferenceLength(text string) int {
	if !strings.HasPrefix(text, "turn") {
		return 0
	}

	i := len("turn")
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}
	if i == len("turn") {
		return 0
	}
	for i < len(text) {
		char := text[i]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' || char == ':' {
			i++
			continue
		}
		break
	}
	return i
}

// SanitizedContentDelta turns a raw upstream delta into the corresponding
// visible delta. It is safe when a citation marker arrives across multiple
// stream chunks.
func SanitizedContentDelta(previousRaw, incomingRaw string) string {
	previousVisible := StripInternalCitationMarkers(previousRaw)
	currentVisible := StripInternalCitationMarkers(previousRaw + incomingRaw)
	if strings.HasPrefix(currentVisible, previousVisible) {
		return strings.TrimPrefix(currentVisible, previousVisible)
	}
	return currentVisible
}

// SanitizedSnapshotDelta is the equivalent operation for legacy events whose
// content is the complete answer-so-far rather than an incremental fragment.
func SanitizedSnapshotDelta(previousRaw, currentRaw string) string {
	previousVisible := StripInternalCitationMarkers(previousRaw)
	currentVisible := StripInternalCitationMarkers(currentRaw)
	if strings.HasPrefix(currentVisible, previousVisible) {
		return strings.TrimPrefix(currentVisible, previousVisible)
	}
	return currentVisible
}
