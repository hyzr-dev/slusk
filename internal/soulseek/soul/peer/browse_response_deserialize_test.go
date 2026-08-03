package peer

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"math"
	"reflect"
	"runtime"
	"testing"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/internal"
)

func browseResponseFrame(t *testing.T, code Code, payload []byte) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	mustWriteUint32(t, buf, uint32(code))
	zw := zlib.NewWriter(buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	frame, err := internal.Pack(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func sharedBrowsePayload(t *testing.T, public, private uint32) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	writeDirectories := func(count uint32) {
		mustWriteUint32(t, buf, count)
		for i := uint32(0); i < count; i++ {
			mustWriteString(t, buf, "")
			mustWriteUint32(t, buf, 0)
		}
	}
	writeDirectories(public)
	mustWriteUint32(t, buf, 0)
	writeDirectories(private)
	return buf.Bytes()
}

func folderBrowsePayload(t *testing.T, folderCount uint32) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	mustWriteUint32(t, buf, 7)
	mustWriteString(t, buf, "root")
	mustWriteUint32(t, buf, folderCount)
	for i := uint32(0); i < folderCount; i++ {
		mustWriteString(t, buf, "")
		mustWriteUint32(t, buf, 0)
	}
	return buf.Bytes()
}

func oneFileDirectoryPayload(t *testing.T, stringSize, attributes uint32) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	mustWriteUint32(t, buf, 1)
	mustWriteString(t, buf, "d")
	mustWriteUint32(t, buf, 1)
	if err := internal.WriteUint8(buf, 1); err != nil {
		t.Fatal(err)
	}
	mustWriteUint32(t, buf, stringSize)
	if stringSize <= maxSharedFileListStringSize {
		buf.Write(bytes.Repeat([]byte{'n'}, int(stringSize)))
	}
	if stringSize > maxSharedFileListStringSize {
		return buf.Bytes()
	}
	if err := internal.WriteUint64(buf, 1); err != nil {
		t.Fatal(err)
	}
	mustWriteString(t, buf, "")
	mustWriteUint32(t, buf, attributes)
	for i := uint32(0); i < attributes && i <= maxSharedFileListAttributes; i++ {
		mustWriteUint32(t, buf, i)
		mustWriteUint32(t, buf, i)
	}
	return buf.Bytes()
}

func TestSharedFileListResponseDeserializeExactLimitsAndTrailingData(t *testing.T) {
	t.Run("aggregate directories", func(t *testing.T) {
		payload := sharedBrowsePayload(t, maxSharedFileListDirectories/2, maxSharedFileListDirectories/2)
		var got SharedFileListResponse
		if err := got.Deserialize(bytes.NewReader(browseResponseFrame(t, CodeSharedFileListResponse, payload))); err != nil {
			t.Fatal(err)
		}
		if len(got.Directories)+len(got.PrivateDirectories) != maxSharedFileListDirectories {
			t.Fatalf("directories = %d", len(got.Directories)+len(got.PrivateDirectories))
		}
	})

	t.Run("string and attributes", func(t *testing.T) {
		payload := bytes.NewBuffer(oneFileDirectoryPayload(t, maxSharedFileListStringSize, maxSharedFileListAttributes))
		mustWriteUint32(t, payload, 0)
		mustWriteUint32(t, payload, 0)
		var got SharedFileListResponse
		if err := got.Deserialize(bytes.NewReader(browseResponseFrame(t, CodeSharedFileListResponse, payload.Bytes()))); err != nil {
			t.Fatal(err)
		}
		if len(got.Directories[0].Files[0].Name) != maxSharedFileListStringSize || len(got.Directories[0].Files[0].Attributes) != maxSharedFileListAttributes {
			t.Fatal("exact limits were not retained")
		}
	})

	t.Run("decompressed size and trailing data", func(t *testing.T) {
		payload := sharedBrowsePayload(t, 0, 0)
		payload = append(payload, make([]byte, maxSharedFileListDecompressedSize-len(payload))...)
		var got SharedFileListResponse
		if err := got.Deserialize(bytes.NewReader(browseResponseFrame(t, CodeSharedFileListResponse, payload))); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSharedFileListResponseDeserializeRejectLimitsBeforeWork(t *testing.T) {
	tests := []struct {
		name string
		body func(*bytes.Buffer)
	}{
		{"directories over", func(b *bytes.Buffer) { mustWriteUint32(t, b, maxSharedFileListDirectories+1) }},
		{"max uint directories", func(b *bytes.Buffer) { mustWriteUint32(t, b, math.MaxUint32) }},
		{"max uint string", func(b *bytes.Buffer) { mustWriteUint32(t, b, 1); mustWriteUint32(t, b, math.MaxUint32) }},
		{"max uint files", func(b *bytes.Buffer) {
			mustWriteUint32(t, b, 1)
			mustWriteString(t, b, "d")
			mustWriteUint32(t, b, math.MaxUint32)
		}},
		{"attributes over", func(b *bytes.Buffer) { b.Write(oneFileDirectoryPayload(t, 0, maxSharedFileListAttributes+1)) }},
		{"max uint attributes", func(b *bytes.Buffer) { b.Write(oneFileDirectoryPayload(t, 0, math.MaxUint32)) }},
		{"string over", func(b *bytes.Buffer) { mustWriteUint32(t, b, 1); mustWriteUint32(t, b, maxSharedFileListStringSize+1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := new(bytes.Buffer)
			tt.body(payload)
			err := new(SharedFileListResponse).Deserialize(bytes.NewReader(browseResponseFrame(t, CodeSharedFileListResponse, payload.Bytes())))
			if !errors.Is(err, soul.ErrMessageTooLarge) {
				t.Fatalf("error = %v, want ErrMessageTooLarge", err)
			}
		})
	}
}

func TestBrowseDirectoryAggregateBudgets(t *testing.T) {
	payload := new(bytes.Buffer)
	mustWriteUint32(t, payload, 2)
	remainingDirectories, remainingFiles := uint32(1), uint32(1)
	if _, err := readBrowseDirectories(payload, 2, &remainingDirectories, &remainingFiles, 2, 2, 32, 1024, "test"); !errors.Is(err, soul.ErrMessageTooLarge) {
		t.Fatalf("directory budget error = %v", err)
	}

	writeDirectory := func(fileCount uint32) *bytes.Buffer {
		buf := new(bytes.Buffer)
		mustWriteString(t, buf, "d")
		mustWriteUint32(t, buf, fileCount)
		for i := uint32(0); i < fileCount; i++ {
			appendFileSearchResponseFile(t, buf, File{Name: "f"})
		}
		return buf
	}
	remainingDirectories, remainingFiles = 3, 2
	for i := 0; i < 2; i++ {
		if _, err := readBrowseDirectories(writeDirectory(1), 1, &remainingDirectories, &remainingFiles, 3, 2, 32, 1024, "test"); err != nil {
			t.Fatalf("exact aggregate file budget: %v", err)
		}
	}
	if _, err := readBrowseDirectories(writeDirectory(1), 1, &remainingDirectories, &remainingFiles, 3, 2, 32, 1024, "test"); !errors.Is(err, soul.ErrMessageTooLarge) {
		t.Fatalf("file budget error = %v", err)
	}
}

func TestFolderContentsResponseDeserializeExactLimits(t *testing.T) {
	t.Run("folders", func(t *testing.T) {
		var got FolderContentsResponse
		if err := got.Deserialize(bytes.NewReader(browseResponseFrame(t, CodeFolderContentsResponse, folderBrowsePayload(t, maxFolderContentsResponseFolders)))); err != nil {
			t.Fatal(err)
		}
		if len(got.Folders) != maxFolderContentsResponseFolders {
			t.Fatalf("folders = %d", len(got.Folders))
		}
	})

	t.Run("strings and attributes", func(t *testing.T) {
		payload := new(bytes.Buffer)
		limitString := bytes.Repeat([]byte{'x'}, maxFolderContentsResponseStringSize)
		writeLimitString := func() {
			mustWriteUint32(t, payload, maxFolderContentsResponseStringSize)
			payload.Write(limitString)
		}
		mustWriteUint32(t, payload, 7)
		writeLimitString() // root
		mustWriteUint32(t, payload, 1)
		writeLimitString() // directory
		mustWriteUint32(t, payload, 1)
		if err := internal.WriteUint8(payload, 1); err != nil {
			t.Fatal(err)
		}
		writeLimitString() // file name
		if err := internal.WriteUint64(payload, 1); err != nil {
			t.Fatal(err)
		}
		writeLimitString() // extension
		mustWriteUint32(t, payload, maxFolderContentsResponseAttributes)
		for i := uint32(0); i < maxFolderContentsResponseAttributes; i++ {
			mustWriteUint32(t, payload, i)
			mustWriteUint32(t, payload, i)
		}

		var got FolderContentsResponse
		if err := got.Deserialize(bytes.NewReader(browseResponseFrame(t, CodeFolderContentsResponse, payload.Bytes()))); err != nil {
			t.Fatal(err)
		}
		file := got.Folders[0].Files[0]
		if len(got.Folder) != maxFolderContentsResponseStringSize || len(got.Folders[0].Name) != maxFolderContentsResponseStringSize || len(file.Name) != maxFolderContentsResponseStringSize || len(file.Extension) != maxFolderContentsResponseStringSize || len(file.Attributes) != maxFolderContentsResponseAttributes {
			t.Fatal("exact folder contents limits were not retained")
		}
	})
}

func TestFolderContentsResponseDeserializeRejectsWireLimits(t *testing.T) {
	filePrefix := func(b *bytes.Buffer) {
		mustWriteUint32(t, b, 7)
		mustWriteString(t, b, "root")
		mustWriteUint32(t, b, 1)
		mustWriteString(t, b, "directory")
		mustWriteUint32(t, b, 1)
		if err := internal.WriteUint8(b, 1); err != nil {
			t.Fatal(err)
		}
	}
	extensionPrefix := func(b *bytes.Buffer) {
		filePrefix(b)
		mustWriteString(t, b, "file")
		if err := internal.WriteUint64(b, 1); err != nil {
			t.Fatal(err)
		}
	}
	attributePrefix := func(b *bytes.Buffer) {
		extensionPrefix(b)
		mustWriteString(t, b, "ext")
	}
	tests := []struct {
		name string
		body func(*bytes.Buffer)
	}{
		{"root string over", func(b *bytes.Buffer) {
			mustWriteUint32(t, b, 7)
			mustWriteUint32(t, b, maxFolderContentsResponseStringSize+1)
		}},
		{"root string max uint", func(b *bytes.Buffer) { mustWriteUint32(t, b, 7); mustWriteUint32(t, b, math.MaxUint32) }},
		{"directory string over", func(b *bytes.Buffer) {
			mustWriteUint32(t, b, 7)
			mustWriteString(t, b, "root")
			mustWriteUint32(t, b, 1)
			mustWriteUint32(t, b, maxFolderContentsResponseStringSize+1)
		}},
		{"file string over", func(b *bytes.Buffer) { filePrefix(b); mustWriteUint32(t, b, maxFolderContentsResponseStringSize+1) }},
		{"extension string over", func(b *bytes.Buffer) {
			extensionPrefix(b)
			mustWriteUint32(t, b, maxFolderContentsResponseStringSize+1)
		}},
		{"folder count over", func(b *bytes.Buffer) {
			mustWriteUint32(t, b, 7)
			mustWriteString(t, b, "root")
			mustWriteUint32(t, b, maxFolderContentsResponseFolders+1)
		}},
		{"folder count max uint", func(b *bytes.Buffer) {
			mustWriteUint32(t, b, 7)
			mustWriteString(t, b, "root")
			mustWriteUint32(t, b, math.MaxUint32)
		}},
		{"file count over", func(b *bytes.Buffer) {
			mustWriteUint32(t, b, 7)
			mustWriteString(t, b, "root")
			mustWriteUint32(t, b, 1)
			mustWriteString(t, b, "directory")
			mustWriteUint32(t, b, maxFolderContentsResponseFiles+1)
		}},
		{"file count max uint", func(b *bytes.Buffer) {
			mustWriteUint32(t, b, 7)
			mustWriteString(t, b, "root")
			mustWriteUint32(t, b, 1)
			mustWriteString(t, b, "directory")
			mustWriteUint32(t, b, math.MaxUint32)
		}},
		{"attributes over", func(b *bytes.Buffer) {
			attributePrefix(b)
			mustWriteUint32(t, b, maxFolderContentsResponseAttributes+1)
		}},
		{"attributes max uint", func(b *bytes.Buffer) { attributePrefix(b); mustWriteUint32(t, b, math.MaxUint32) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := new(bytes.Buffer)
			tt.body(payload)
			err := new(FolderContentsResponse).Deserialize(bytes.NewReader(browseResponseFrame(t, CodeFolderContentsResponse, payload.Bytes())))
			if !errors.Is(err, soul.ErrMessageTooLarge) {
				t.Fatalf("error = %v, want ErrMessageTooLarge", err)
			}
		})
	}
}

func TestFolderContentsResponseExactFileLimitTruncatedDoesNotPreallocate(t *testing.T) {
	payload := new(bytes.Buffer)
	mustWriteUint32(t, payload, 7)
	mustWriteString(t, payload, "root")
	mustWriteUint32(t, payload, 1)
	mustWriteString(t, payload, "directory")
	mustWriteUint32(t, payload, maxFolderContentsResponseFiles)
	frame := browseResponseFrame(t, CodeFolderContentsResponse, payload.Bytes())

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := new(FolderContentsResponse).Deserialize(bytes.NewReader(frame))
	runtime.ReadMemStats(&after)
	if err == nil || errors.Is(err, soul.ErrMessageTooLarge) {
		t.Fatalf("error = %v, want a normal truncated-payload parse error", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Fatalf("truncated exact-limit file count allocated %d bytes", allocated)
	}
}

func TestBrowseResponsesRejectDecompressionOverflowCorruptionAndTruncation(t *testing.T) {
	for _, code := range []Code{CodeSharedFileListResponse, CodeFolderContentsResponse} {
		t.Run(code.String(), func(t *testing.T) {
			limit := maxSharedFileListDecompressedSize
			frame := browseResponseFrame(t, code, make([]byte, limit+1))
			var err error
			if code == CodeSharedFileListResponse {
				err = new(SharedFileListResponse).Deserialize(bytes.NewReader(frame))
			} else {
				err = new(FolderContentsResponse).Deserialize(bytes.NewReader(frame))
			}
			if !errors.Is(err, soul.ErrMessageTooLarge) {
				t.Fatalf("overflow error = %v", err)
			}

			validPayload := sharedBrowsePayload(t, 0, 0)
			if code == CodeFolderContentsResponse {
				validPayload = folderBrowsePayload(t, 0)
			}
			frame = browseResponseFrame(t, code, validPayload)
			frame[len(frame)-1] ^= 0xff
			if code == CodeSharedFileListResponse {
				err = new(SharedFileListResponse).Deserialize(bytes.NewReader(frame))
			} else {
				err = new(FolderContentsResponse).Deserialize(bytes.NewReader(frame))
			}
			if err == nil {
				t.Fatal("corrupt checksum accepted")
			}

			frame = browseResponseFrame(t, code, validPayload)
			frame = frame[:len(frame)-2]
			if code == CodeSharedFileListResponse {
				err = new(SharedFileListResponse).Deserialize(bytes.NewReader(frame))
			} else {
				err = new(FolderContentsResponse).Deserialize(bytes.NewReader(frame))
			}
			if err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("truncated zlib error = %v", err)
			}
		})
	}
}

func TestBrowseResponsesReplaceReceiverOnlyOnSuccess(t *testing.T) {
	sharedOriginal := SharedFileListResponse{Directories: []Directory{{Name: "old"}}}
	shared := sharedOriginal
	badShared := browseResponseFrame(t, CodeSharedFileListResponse, []byte{1, 0})
	if err := shared.Deserialize(bytes.NewReader(badShared)); err == nil {
		t.Fatal("malformed shared payload accepted")
	}
	if !reflect.DeepEqual(shared, sharedOriginal) {
		t.Fatalf("shared receiver changed on error: %#v", shared)
	}
	if err := shared.Deserialize(bytes.NewReader(browseResponseFrame(t, CodeSharedFileListResponse, sharedBrowsePayload(t, 0, 0)))); err != nil {
		t.Fatal(err)
	}
	if len(shared.Directories) != 0 {
		t.Fatalf("shared reuse retained stale state: %#v", shared)
	}

	folderOriginal := FolderContentsResponse{Token: 99, Folder: "old", Folders: []Directory{{Name: "old"}}}
	folder := folderOriginal
	badFolder := browseResponseFrame(t, CodeFolderContentsResponse, []byte{1, 0})
	if err := folder.Deserialize(bytes.NewReader(badFolder)); err == nil {
		t.Fatal("malformed folder payload accepted")
	}
	if !reflect.DeepEqual(folder, folderOriginal) {
		t.Fatalf("folder receiver changed on error: %#v", folder)
	}
	if err := folder.Deserialize(bytes.NewReader(browseResponseFrame(t, CodeFolderContentsResponse, folderBrowsePayload(t, 0)))); err != nil {
		t.Fatal(err)
	}
	if folder.Token != 7 || folder.Folder != "root" || len(folder.Folders) != 0 {
		t.Fatalf("folder reuse retained stale state: %#v", folder)
	}
}
