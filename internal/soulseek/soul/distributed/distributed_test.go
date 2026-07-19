package distributed

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
)

func TestSearchFramingAndRoundTrip(t *testing.T) {
	want := Search{Username: "alice", Token: soul.Token(0x78563412), Query: "rare mix"}

	frame, err := want.Serialize(&want)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	if got := binary.LittleEndian.Uint32(frame[:4]); got != uint32(len(frame)-4) {
		t.Fatalf("declared size = %d, want %d", got, len(frame)-4)
	}
	if got := frame[4]; got != byte(CodeSearch) {
		t.Fatalf("code = %d, want %d", got, CodeSearch)
	}
	if got := binary.LittleEndian.Uint32(frame[5:9]); got != uint32('1') {
		t.Fatalf("identifier = %d, want ASCII '1' (49)", got)
	}

	var got Search
	if err := got.Deserialize(bytes.NewReader(frame)); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestSearchDeserializeRejectsInvalidIdentifier(t *testing.T) {
	valid := Search{Username: "alice", Token: 7, Query: "track"}
	frame, err := valid.Serialize(&valid)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	for _, identifier := range []uint32{0, '0', '2', ^uint32(0)} {
		t.Run(string(rune(identifier)), func(t *testing.T) {
			invalid := append([]byte(nil), frame...)
			binary.LittleEndian.PutUint32(invalid[5:9], identifier)

			var search Search
			err := search.Deserialize(bytes.NewReader(invalid))
			if !errors.Is(err, errInvalidSearchIdentifier) {
				t.Fatalf("Deserialize error = %v, want invalid-identifier error", err)
			}
		})
	}
}

func TestBranchLevelFramingAndRoundTrip(t *testing.T) {
	want := BranchLevel{Level: -2}
	frame, err := want.Serialize(&want)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	wantFrame := []byte{5, 0, 0, 0, byte(CodeBranchLevel), 0xfe, 0xff, 0xff, 0xff}
	if !bytes.Equal(frame, wantFrame) {
		t.Fatalf("frame = %x, want %x", frame, wantFrame)
	}

	var got BranchLevel
	if err := got.Deserialize(bytes.NewReader(frame)); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestBranchRootFramingAndRoundTrip(t *testing.T) {
	want := BranchRoot{Root: "branch-root"}
	frame, err := want.Serialize(&want)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	if got := binary.LittleEndian.Uint32(frame[:4]); got != uint32(1+4+len(want.Root)) {
		t.Fatalf("declared size = %d, want %d", got, 1+4+len(want.Root))
	}
	if frame[4] != byte(CodeBranchRoot) {
		t.Fatalf("code = %d, want %d", frame[4], CodeBranchRoot)
	}
	if got := binary.LittleEndian.Uint32(frame[5:9]); got != uint32(len(want.Root)) {
		t.Fatalf("root length = %d, want %d", got, len(want.Root))
	}
	if got := string(frame[9:]); got != want.Root {
		t.Fatalf("root bytes = %q, want %q", got, want.Root)
	}

	var got BranchRoot
	if err := got.Deserialize(bytes.NewReader(frame)); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestEmbeddedMessageFramingPreservesRawBytes(t *testing.T) {
	rawSearchBody := []byte{49, 0, 0, 0, 1, 0, 0, 0, 'u', 7, 0, 0, 0, 1, 0, 0, 0, 'q', 0xff, 0x00}
	want := EmbeddedMessage{Code: CodeSearch, Message: rawSearchBody}
	frame, err := want.Serialize(&want)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	if got := binary.LittleEndian.Uint32(frame[:4]); got != uint32(1+1+4+len(rawSearchBody)) {
		t.Fatalf("declared size = %d, want %d", got, 1+1+4+len(rawSearchBody))
	}
	if frame[4] != byte(CodeEmbeddedMessage) || frame[5] != byte(CodeSearch) {
		t.Fatalf("outer/embedded codes = %d/%d, want %d/%d", frame[4], frame[5], CodeEmbeddedMessage, CodeSearch)
	}
	if got := binary.LittleEndian.Uint32(frame[6:10]); got != uint32(len(rawSearchBody)) {
		t.Fatalf("embedded length = %d, want %d", got, len(rawSearchBody))
	}
	if !bytes.Equal(frame[10:], rawSearchBody) {
		t.Fatalf("embedded bytes = %x, want %x", frame[10:], rawSearchBody)
	}

	var got EmbeddedMessage
	if err := got.Deserialize(bytes.NewReader(frame)); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestDistributedReadKeepsFrameBoundaries(t *testing.T) {
	search := Search{Username: "alice", Token: 1, Query: "one"}
	searchFrame, err := search.Serialize(&search)
	if err != nil {
		t.Fatalf("serialize search: %v", err)
	}
	level := BranchLevel{Level: 3}
	levelFrame, err := level.Serialize(&level)
	if err != nil {
		t.Fatalf("serialize branch level: %v", err)
	}

	stream := bytes.NewBuffer(append(searchFrame, levelFrame...))
	reader, size, code, err := Read(stream)
	if err != nil {
		t.Fatalf("read search frame: %v", err)
	}
	if size != len(searchFrame)-4 || code != CodeSearch {
		t.Fatalf("search size/code = %d/%d, want %d/%d", size, code, len(searchFrame)-4, CodeSearch)
	}
	var gotSearch Search
	if err := gotSearch.Deserialize(reader); err != nil {
		t.Fatalf("deserialize search: %v", err)
	}

	reader, size, code, err = Read(stream)
	if err != nil {
		t.Fatalf("read branch-level frame: %v", err)
	}
	if size != len(levelFrame)-4 || code != CodeBranchLevel {
		t.Fatalf("level size/code = %d/%d, want %d/%d", size, code, len(levelFrame)-4, CodeBranchLevel)
	}
	var gotLevel BranchLevel
	if err := gotLevel.Deserialize(reader); err != nil {
		t.Fatalf("deserialize branch level: %v", err)
	}
	if stream.Len() != 0 {
		t.Fatalf("unread stream bytes = %d, want 0", stream.Len())
	}
}
