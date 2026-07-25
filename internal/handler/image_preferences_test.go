package handler

import (
	"strings"
	"testing"
)

func TestImagePromptWithPreferences(t *testing.T) {
	prompt, err := imagePromptWithPreferences("a mountain", "1024x1024", "high")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"a mountain", "1024x1024", "high"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt %q does not contain %q", prompt, want)
		}
	}
}

func TestImagePromptWithPreferencesRejectsInvalidValues(t *testing.T) {
	if _, err := imagePromptWithPreferences("test", "999x999", ""); err == nil {
		t.Fatal("invalid size was accepted")
	}
	if _, err := imagePromptWithPreferences("test", "", "ultra"); err == nil {
		t.Fatal("invalid quality was accepted")
	}
}
