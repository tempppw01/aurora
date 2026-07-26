package chatgpt

import "testing"

func TestStripInternalCitationMarkers(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "complete marker",
			text: "市场\uE200cite\uE202turn0search0\uE201特征",
			want: "市场特征",
		},
		{
			name: "marker without closing control character",
			text: "市场\uE200cite\uE202turn0特征",
			want: "市场特征",
		},
		{
			name: "keeps ordinary cite text",
			text: "请 cite 这篇文章。",
			want: "请 cite 这篇文章。",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := StripInternalCitationMarkers(test.text); got != test.want {
				t.Fatalf("StripInternalCitationMarkers(%q) = %q, want %q", test.text, got, test.want)
			}
		})
	}
}

func TestSanitizedContentDeltaHandlesSplitCitationMarker(t *testing.T) {
	firstRaw := "市场\uE200cite"
	if got := SanitizedContentDelta("市场", "\uE200cite"); got != "" {
		t.Fatalf("first partial marker delta = %q, want empty", got)
	}
	if got := SanitizedContentDelta(firstRaw, "\uE202turn0search0\uE201特征"); got != "特征" {
		t.Fatalf("completed marker delta = %q, want %q", got, "特征")
	}
}

func TestStripInternalCitationMarkersConvertsEntityMarkers(t *testing.T) {
	raw := "\uE200entity\uE202[\"book\",\"西游记\",\"中国古典小说\"]\uE201是四大名著之一"
	if got := StripInternalCitationMarkers(raw); got != "西游记是四大名著之一" {
		t.Fatalf("StripInternalCitationMarkers(%q) = %q", raw, got)
	}
}

func TestSanitizedContentDeltaHandlesSplitEntityMarker(t *testing.T) {
	firstRaw := "\uE200entity\uE202[\"book\",\"西游记\""
	if got := SanitizedContentDelta("", firstRaw); got != "" {
		t.Fatalf("partial entity delta = %q, want empty", got)
	}
	if got := SanitizedContentDelta(firstRaw, ",\"中国古典小说\"]\uE201是四大名著之一"); got != "西游记是四大名著之一" {
		t.Fatalf("completed entity delta = %q", got)
	}
}

func TestSanitizedSnapshotDelta(t *testing.T) {
	previousRaw := "市场"
	currentRaw := "市场\uE200cite\uE202turn0search0\uE201特征"
	if got := SanitizedSnapshotDelta(previousRaw, currentRaw); got != "特征" {
		t.Fatalf("snapshot delta = %q, want %q", got, "特征")
	}
}
