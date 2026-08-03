package peer

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/internal"
)

const CodeFolderContentsResponse Code = 37

const (
	maxFolderContentsResponseDecompressedSize = 16 << 20
	maxFolderContentsResponseFolders          = 100_000
	maxFolderContentsResponseFiles            = 1_000_000
	maxFolderContentsResponseAttributes       = 32
	maxFolderContentsResponseStringSize       = 1 << 20
)

// FolderContentsResponse code 37 peer responds with the contents of a
// particular folder (with all subfolders) after we’ve sent a FolderContentsRequest.
type FolderContentsResponse struct {
	Token   soul.Token
	Folder  string
	Folders []Directory
}

// Serialize accepts a FolderContentsResponse and returns a message packed as a byte slice.
func (f *FolderContentsResponse) Serialize(message *FolderContentsResponse) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := internal.WriteUint32(buf, uint32(CodeFolderContentsResponse))
	if err != nil {
		return nil, err
	}

	zw := zlib.NewWriter(buf)

	err = internal.WriteUint32(zw, uint32(message.Token))
	if err != nil {
		return nil, err
	}

	err = internal.WriteString(zw, message.Folder)
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(zw, uint32(len(message.Folders)))
	if err != nil {
		return nil, err
	}

	for _, f := range message.Folders {
		err = internal.WriteString(zw, f.Name)
		if err != nil {
			return nil, err
		}

		err = internal.WriteUint32(zw, uint32(len(f.Files)))
		if err != nil {
			return nil, err
		}

		for _, file := range f.Files {
			err = internal.WriteUint8(zw, uint8(1))
			if err != nil {
				return nil, err
			}

			err = internal.WriteString(zw, file.Name)
			if err != nil {
				return nil, err
			}

			err = internal.WriteUint64(zw, file.Size)
			if err != nil {
				return nil, err
			}

			err = internal.WriteString(zw, file.Extension)
			if err != nil {
				return nil, err
			}

			err = internal.WriteUint32(zw, uint32(len(file.Attributes)))
			if err != nil {
				return nil, err
			}

			for _, attribute := range file.Attributes {
				err = internal.WriteUint32(zw, uint32(attribute.Code))
				if err != nil {
					return nil, err
				}

				err = internal.WriteUint32(zw, uint32(attribute.Value))
				if err != nil {
					return nil, err
				}
			}
		}
	}

	err = zw.Close()
	if err != nil {
		return nil, err
	}

	return internal.Pack(buf.Bytes())
}

// Deserialize populates a FolderContentsResponse with the data in the provided reader.
func (f *FolderContentsResponse) Deserialize(reader io.Reader) error {
	if _, err := internal.ReadUint32(reader); err != nil { // size
		return err
	}

	code, err := internal.ReadUint32(reader)
	if err != nil {
		return err
	}
	if code != uint32(CodeFolderContentsResponse) {
		return errors.Join(soul.ErrMismatchingCodes,
			fmt.Errorf("expected code %d, got %d", CodeFolderContentsResponse, code))
	}

	decompressed, err := readBoundedBrowsePayload(reader, maxFolderContentsResponseDecompressedSize, "folder contents response")
	if err != nil {
		return err
	}
	payload := bytes.NewReader(decompressed)
	var decoded FolderContentsResponse
	decoded.Token, err = internal.ReadUint32ToToken(payload)
	if err != nil {
		return err
	}
	decoded.Folder, err = readBrowseString(payload, maxFolderContentsResponseStringSize, "folder contents response")
	if err != nil {
		return err
	}
	folderCount, err := internal.ReadUint32(payload)
	if err != nil {
		return err
	}
	remainingFolders := uint32(maxFolderContentsResponseFolders)
	remainingFiles := uint32(maxFolderContentsResponseFiles)
	decoded.Folders, err = readBrowseDirectories(payload, folderCount, &remainingFolders, &remainingFiles, maxFolderContentsResponseFolders, maxFolderContentsResponseFiles, maxFolderContentsResponseAttributes, maxFolderContentsResponseStringSize, "folder contents response")
	if err != nil {
		return err
	}

	*f = decoded
	return nil
}
