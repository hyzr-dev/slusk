package peer

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
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
			name: "decompressed size",
			response: func() *SharedFileListResponse {
				name := strings.Repeat("a", maxSharedFileListStringSize)
				files := make([]File, 17)
				for i := range files {
					files[i].Name = name
				}
				return &SharedFileListResponse{Directories: []Directory{{Name: "Music", Files: files}}}
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.response.Serialize(tt.response); !errors.Is(err, soul.ErrMessageTooLarge) {
				t.Fatalf("Serialize error = %v, want ErrMessageTooLarge", err)
			}
		})
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
