package peer

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul"
)

func TestSharedFileListResponseAllowsEmptyAndFilesystemEdgeCases(t *testing.T) {
	for _, original := range []*SharedFileListResponse{
		{},
		{Directories: []Directory{{Name: "Empty"}, {Name: "Files", Files: []File{{Name: "zero", Size: 0, Extension: ""}}}}},
	} {
		wire, err := original.Serialize(original)
		if err != nil {
			t.Fatalf("Serialize(%#v): %v", original, err)
		}
		var decoded SharedFileListResponse
		if err := decoded.Deserialize(bytes.NewReader(wire)); err != nil {
			t.Fatalf("Deserialize: %v", err)
		}
		if len(decoded.Directories) != len(original.Directories) {
			t.Fatalf("directory count = %d, want %d", len(decoded.Directories), len(original.Directories))
		}
	}
}

func TestSharedFileListResponseEnforcesCountAndSerializedSizeBounds(t *testing.T) {
	tests := []struct {
		name     string
		response *SharedFileListResponse
	}{
		{
			name:     "directory count",
			response: &SharedFileListResponse{Directories: make([]Directory, maxSharedFileListDirectories+1)},
		},
		{
			name: "attribute count",
			response: &SharedFileListResponse{Directories: []Directory{{Name: "Music", Files: []File{{
				Name: "track.flac", Attributes: make([]Attribute, maxSharedFileListAttributes+1),
			}}}}},
		},
		{
			// Highly compressible names: the frame stays tiny, so only the
			// serialized-payload valve can trip here. That is the pathology it
			// exists for - the file and directory counts are both far under
			// their own limits (issue #409).
			name: "serialized size",
			response: func() *SharedFileListResponse {
				name := strings.Repeat("a", maxSharedFileListStringSize)
				files := make([]File, maxSharedFileListSerializedSize/maxSharedFileListStringSize+1)
				for i := range files {
					files[i].Name = name
				}
				return &SharedFileListResponse{Directories: []Directory{{Name: "Music", Files: files}}}
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.response.Serialize(tt.response)
			if !errors.Is(err, soul.ErrMessageTooLarge) {
				t.Fatalf("Serialize error = %v, want ErrMessageTooLarge", err)
			}
			// The limit that was hit has to be in the message, not just the
			// fact that one was: internal/soulseek quotes this error verbatim
			// to a user whose share is too big to publish (issue #408), and
			// "too large" without a number is not something they can act on.
			if want := fmt.Sprint(maxSharedFileListSerializedSize); tt.name == "serialized size" && !strings.Contains(err.Error(), want) {
				t.Fatalf("Serialize error = %q, want it to name the %s-byte limit", err, want)
			}
		})
	}
}

// TestSharedFileListResponseSerializesHalfMillionFiles is the case that opened
// issue #409: a user asked whether slusk can publish a large library, and the
// single 16 MiB cap shared by both directions capped it at roughly 170,000
// files. This fails on that cap and passes on the split one.
//
// The frame-budget assertion is a sanity check, not a tight bound - realistic
// paths compress far enough that half a million files land two orders of
// magnitude inside it. What squeezes the budget is entropy, not file count, so
// this cannot stand in for a test of the frame limiter itself.
func TestSharedFileListResponseSerializesHalfMillionFiles(t *testing.T) {
	const (
		directories      = 5_000
		filesPerDirector = 100
	)
	share := &SharedFileListResponse{Directories: make([]Directory, directories)}
	for d := range share.Directories {
		files := make([]File, filesPerDirector)
		for f := range files {
			files[f] = File{
				Name:      fmt.Sprintf(`%02d - Track Title Number %d.flac`, f+1, f+1),
				Size:      uint64(4_000_000 + f),
				Extension: "flac",
				Attributes: []Attribute{
					{Code: Bitrate, Value: 1024},
					{Code: Duration, Value: uint32(180 + f)},
				},
			}
		}
		share.Directories[d] = Directory{
			Name:  fmt.Sprintf(`Music\Artist %04d\Album %04d (2026) [FLAC]`, d, d),
			Files: files,
		}
	}

	frame, err := share.Serialize(share)
	if err != nil {
		t.Fatalf("Serialize %d files: %v", directories*filesPerDirector, err)
	}
	if budget := int64(MaxOrdinaryFrameSize) + 4; int64(len(frame)) > budget {
		t.Fatalf("frame = %d bytes, exceeds the %d-byte session write budget", len(frame), budget)
	}
}

func TestFolderAndSearchResponsesAllowEmptyEdgeCases(t *testing.T) {
	folder := &FolderContentsResponse{Token: 4, Folder: "missing"}
	wire, err := folder.Serialize(folder)
	if err != nil {
		t.Fatal(err)
	}
	var decodedFolder FolderContentsResponse
	if err := decodedFolder.Deserialize(bytes.NewReader(wire)); err != nil || decodedFolder.Token != 4 || len(decodedFolder.Folders) != 0 {
		t.Fatalf("folder round trip = %#v, %v", decodedFolder, err)
	}

	search := &FileSearchResponse{Username: "me", Token: 5, Results: []File{{Name: `Music\zero`, Size: 0}}}
	wire, err = search.Serialize(search)
	if err != nil {
		t.Fatal(err)
	}
	var decodedSearch FileSearchResponse
	if err := decodedSearch.Deserialize(bytes.NewReader(wire)); err != nil || len(decodedSearch.Results) != 1 || decodedSearch.Results[0].Size != 0 {
		t.Fatalf("search round trip = %#v, %v", decodedSearch, err)
	}
}
