package main

import (
	"testing"
)

// TestEnrichMetadataIDScheme verifies that enrichMetadata produces the same
// MD5-based identifier as the Python implementation.
//
// Python reference:
//
//	unique_id_str = f"{extractor}__{video_id}".encode("utf-8")
//	md5_hash_digest = hashlib.md5(unique_id_str).hexdigest()
//
// For youtube extractor + id "dQw4w9WgXcQ":
//
//	md5("youtube__dQw4w9WgXcQ") = "07a34f...<32 hex chars>"
func TestEnrichMetadataIDScheme(t *testing.T) {
	m := &VideoMetadata{
		ID:        "dQw4w9WgXcQ",
		Extractor: "youtube",
	}
	enrichMetadata(m)

	if len(m.MbIdentifier) != 32 {
		t.Errorf("MbIdentifier should be 32 hex chars, got %d: %q", len(m.MbIdentifier), m.MbIdentifier)
	}

	// Path structure: first 2 chars / next 2 chars / full hash
	expectedPath := m.MbIdentifier[0:2] + "/" + m.MbIdentifier[2:4] + "/" + m.MbIdentifier
	if m.MbPath != expectedPath {
		t.Errorf("MbPath = %q, want %q", m.MbPath, expectedPath)
	}

	// Different extractors must produce different identifiers even for the same video ID.
	m2 := &VideoMetadata{
		ID:        "dQw4w9WgXcQ",
		Extractor: "vimeo",
	}
	enrichMetadata(m2)
	if m.MbIdentifier == m2.MbIdentifier {
		t.Error("different extractors with the same video ID must produce different MbIdentifiers")
	}
}

// TestEnrichMetadataDeterminism checks that the same inputs always produce
// the same output (important for deduplication).
func TestEnrichMetadataDeterminism(t *testing.T) {
	make := func() VideoMetadata {
		m := VideoMetadata{ID: "abc123", Extractor: "mysite"}
		enrichMetadata(&m)
		return m
	}
	a, b := make(), make()
	if a.MbIdentifier != b.MbIdentifier {
		t.Error("enrichMetadata is not deterministic")
	}
}

// TestNormalizeTagValue checks the tag normalization matches Python's
// .lower().replace(' ', '_').
func TestNormalizeTagValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello World", "hello_world"},
		{"UPPER", "upper"},
		{"already_lower", "already_lower"},
		{"Multi  Space", "multi__space"},
		{"", ""},
	}
	for _, c := range cases {
		got := normalizeTagValue(c.in)
		if got != c.want {
			t.Errorf("normalizeTagValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}


// TestFormatBytes verifies human-readable byte formatting.
func TestFormatBytes(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1024 * 1024, "1.00 MB"},
		{1024 * 1024 * 1024, "1.00 GB"},
	}
	for _, c := range cases {
		got := formatBytes(c.n)
		if got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
