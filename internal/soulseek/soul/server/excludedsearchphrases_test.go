package server

import (
	"bytes"
	"testing"
)

// TestExcludedSearchPhrasesDeserialize confirms code-160 decodes a list of
// excluded phrases in order (issue #319: the client needs these to stop
// issuing searches every peer is instructed to ignore).
func TestExcludedSearchPhrasesDeserialize(t *testing.T) {
	var payload bytes.Buffer
	putWireUint32(&payload, uint32(CodeExcludedSearchPhrases))
	putWireUint32(&payload, 2) // number of phrases
	putWireString(&payload, "bob dylan")
	putWireString(&payload, "some other phrase")

	var msg ExcludedSearchPhrases
	if err := msg.Deserialize(bytes.NewReader(packServerWire(&payload))); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	want := []string{"bob dylan", "some other phrase"}
	if len(msg.Phrases) != len(want) {
		t.Fatalf("Phrases = %v, want %v", msg.Phrases, want)
	}
	for i := range want {
		if msg.Phrases[i] != want[i] {
			t.Errorf("Phrases[%d] = %q, want %q", i, msg.Phrases[i], want[i])
		}
	}
}

// TestExcludedSearchPhrasesDeserializeEmpty confirms a zero-count list
// decodes to an empty (nil) slice without error.
func TestExcludedSearchPhrasesDeserializeEmpty(t *testing.T) {
	var payload bytes.Buffer
	putWireUint32(&payload, uint32(CodeExcludedSearchPhrases))
	putWireUint32(&payload, 0) // no phrases

	var msg ExcludedSearchPhrases
	if err := msg.Deserialize(bytes.NewReader(packServerWire(&payload))); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if len(msg.Phrases) != 0 {
		t.Errorf("Phrases = %v, want empty", msg.Phrases)
	}
}

// TestExcludedSearchPhrasesDeserializeWrongCode confirms a mismatched code
// is rejected rather than silently misparsed.
func TestExcludedSearchPhrasesDeserializeWrongCode(t *testing.T) {
	var payload bytes.Buffer
	putWireUint32(&payload, uint32(CodeWatchUser)) // wrong code
	putWireUint32(&payload, 0)

	var msg ExcludedSearchPhrases
	if err := msg.Deserialize(bytes.NewReader(packServerWire(&payload))); err == nil {
		t.Fatal("Deserialize accepted a mismatched code, want rejection")
	}
}
