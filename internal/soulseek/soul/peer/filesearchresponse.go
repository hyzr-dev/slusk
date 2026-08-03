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

const CodeFileSearchResponse Code = 9

const (
	maxFileSearchResponseDecompressedSize = 16 << 20
	maxFileSearchResponseFiles            = 4096
	maxFileSearchResponseAttributes       = 32
	maxFileSearchResponseStringSize       = 1 << 20
)

var errUnexpectedFileSearchResponseData = errors.New("unexpected data after file search response")

// FileSearchResponse code 9 peer sends this message when it has a file search match.
// The token is taken from original FileSearch, UserSearch or RoomSearch server message.
type FileSearchResponse struct {
	Username       string
	Token          soul.Token
	Results        []File
	FreeSlot       bool
	AverageSpeed   int
	Queue          int // Queue is the length of the queued transfers.
	PrivateResults []File
}

// Serialize accepts a FileSearchResponse and returns a message packed as a byte slice.
func (f *FileSearchResponse) Serialize(fs *FileSearchResponse) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := internal.WriteUint32(buf, uint32(CodeFileSearchResponse))
	if err != nil {
		return nil, err
	}

	zw := zlib.NewWriter(buf)

	err = internal.WriteString(zw, fs.Username)
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(zw, uint32(fs.Token))
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(zw, uint32(len(fs.Results)))
	if err != nil {
		return nil, err
	}

	err = f.walkWrite(zw, fs.Results)
	if err != nil {
		return nil, err
	}

	err = internal.WriteBool(zw, fs.FreeSlot)
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(zw, uint32(fs.AverageSpeed))
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(zw, uint32(fs.Queue))
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(zw, uint32(0))
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(zw, uint32(len(fs.PrivateResults)))
	if err != nil {
		return nil, err
	}

	err = f.walkWrite(zw, fs.PrivateResults)
	if err != nil {
		return nil, err
	}

	err = zw.Close()
	if err != nil {
		return nil, err
	}

	return internal.Pack(buf.Bytes())
}

func (FileSearchResponse) walkWrite(zw *zlib.Writer, files []File) error {
	for _, file := range files {
		err := internal.WriteUint8(zw, uint8(1))
		if err != nil {
			return err
		}

		if file.Name == "" {
			return ErrEmptyFileName
		}

		err = internal.WriteString(zw, file.Name)
		if err != nil {
			return err
		}

		err = internal.WriteUint64(zw, file.Size)
		if err != nil {
			return err
		}

		err = internal.WriteString(zw, file.Extension)
		if err != nil {
			return err
		}

		err = internal.WriteUint32(zw, uint32(len(file.Attributes)))
		if err != nil {
			return err
		}

		for _, attribute := range file.Attributes {
			err = internal.WriteUint32(zw, uint32(attribute.Code))
			if err != nil {
				return err
			}

			err = internal.WriteUint32(zw, attribute.Value)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// Deserialize populates a FileSearchResponse with the data in the provided reader.
func (f *FileSearchResponse) Deserialize(reader io.Reader) error {
	_, err := internal.ReadUint32(reader) // size
	if err != nil {
		return err
	}

	code, err := internal.ReadUint32(reader) // code 9
	if err != nil {
		return err
	}

	if code != uint32(CodeFileSearchResponse) {
		return errors.Join(soul.ErrMismatchingCodes,
			fmt.Errorf("expected code %d, got %d", CodeFileSearchResponse, code))
	}

	decompressed, err := readFileSearchResponsePayload(reader)
	if err != nil {
		return err
	}

	payload := bytes.NewReader(decompressed)
	*f = FileSearchResponse{}

	f.Username, err = readFileSearchResponseString(payload)
	if err != nil {
		return err
	}

	f.Token, err = internal.ReadUint32ToToken(payload)
	if err != nil {
		return err
	}

	results, err := internal.ReadUint32(payload)
	if err != nil {
		return err
	}

	f.Results, err = f.walkRead(results, payload)
	if err != nil {
		return err
	}

	f.FreeSlot, err = internal.ReadBool(payload)
	if err != nil {
		return err
	}

	f.AverageSpeed, err = internal.ReadUint32ToInt(payload)
	if err != nil {
		return err
	}

	f.Queue, err = internal.ReadUint32ToInt(payload)
	if err != nil {
		return err
	}

	// Nicotine+ treats both tail components independently as optional when no
	// decompressed content remains. Only a clean EOF at a field boundary is
	// accepted; a partial uint32 or a truncated declared file remains an error.
	_, err = internal.ReadUint32(payload)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}

	privateResults, err := internal.ReadUint32(payload)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}

	f.PrivateResults, err = f.walkRead(privateResults, payload)
	if err != nil {
		return err
	}

	if payload.Len() != 0 {
		return errUnexpectedFileSearchResponseData
	}

	return nil
}

func readFileSearchResponsePayload(reader io.Reader) ([]byte, error) {
	zr, err := zlib.NewReader(reader)
	if err != nil {
		return nil, err
	}

	decompressed, readErr := io.ReadAll(io.LimitReader(zr, maxFileSearchResponseDecompressedSize+1))
	closeErr := zr.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(decompressed) > maxFileSearchResponseDecompressedSize {
		return nil, fmt.Errorf("%w: decompressed file search response exceeds %d bytes", soul.ErrMessageTooLarge, maxFileSearchResponseDecompressedSize)
	}

	return decompressed, nil
}

func readFileSearchResponseString(reader io.Reader) (string, error) {
	size, err := internal.ReadStringLen(reader)
	if err != nil {
		return "", err
	}
	if size > maxFileSearchResponseStringSize {
		return "", fmt.Errorf("%w: file search response string exceeds %d bytes", soul.ErrMessageTooLarge, maxFileSearchResponseStringSize)
	}

	return internal.ReadStringBody(reader, size)
}

func (f *FileSearchResponse) walkRead(numberOfFiles uint32, reader io.Reader) ([]File, error) {
	if numberOfFiles > maxFileSearchResponseFiles {
		return nil, fmt.Errorf("%w: file search response contains more than %d files", soul.ErrMessageTooLarge, maxFileSearchResponseFiles)
	}

	var files []File
	if numberOfFiles > 0 {
		files = make([]File, 0, numberOfFiles)
	}
	for i := uint32(0); i < numberOfFiles; i++ {
		var file File

		if _, err := internal.ReadUint8(reader); err != nil {
			return nil, err
		}

		var err error
		file.Name, err = readFileSearchResponseString(reader)
		if err != nil {
			return nil, err
		}

		file.Size, err = internal.ReadUint64(reader)
		if err != nil {
			return nil, err
		}

		file.Extension, err = readFileSearchResponseString(reader)
		if err != nil {
			return nil, err
		}

		attributes, err := internal.ReadUint32(reader)
		if err != nil {
			return nil, err
		}
		if attributes > maxFileSearchResponseAttributes {
			return nil, fmt.Errorf("%w: file search response file contains more than %d attributes", soul.ErrMessageTooLarge, maxFileSearchResponseAttributes)
		}

		if attributes > 0 {
			file.Attributes = make([]Attribute, 0, attributes)
		}
		for j := uint32(0); j < attributes; j++ {
			code, err := internal.ReadUint32(reader)
			if err != nil {
				return nil, err
			}

			value, err := internal.ReadUint32(reader)
			if err != nil {
				return nil, err
			}

			file.Attributes = append(file.Attributes, Attribute{
				Code:  FileAttributeType(code),
				Value: value,
			})
		}

		files = append(files, file)
	}

	return files, nil
}
