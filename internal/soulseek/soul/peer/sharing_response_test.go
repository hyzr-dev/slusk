package peer

import (
	"bytes"
	"testing"
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
