package peer

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/internal"
)

const CodeSharedFileListResponse Code = 5

// MaxOrdinaryFrameSize is the largest frame an ordinary peer ('P') connection
// will carry, excluding the 4-byte size prefix. It bounds traffic in *both*
// directions, and the two directions carry different risk:
//
//   - Outbound, the session layer derives its write budget from this, so a
//     serializer here that bounds itself by the same constant cannot build a
//     frame the sender will then refuse.
//   - Inbound, it is the read limit for every ordinary peer frame and the
//     per-session queue budget. Raising it therefore also raises how many
//     bytes an unauthenticated remote peer can make this process buffer, on
//     every peer message type at once - not just browse.
//
// Weigh the second bullet before raising it to publish a larger share.
const MaxOrdinaryFrameSize = 16 << 20

const (
	// maxSharedFileListSerializedSize bounds only what we encode ourselves. It
	// is not a protection - an oversized outgoing browse list is a risk to our
	// peers, never to us - it is a backstop against pathological path lengths,
	// which are the one shape that can slip past both the file count and the
	// compressed frame. Neither Nicotine+ nor Soulseek.NET bounds its own
	// outgoing list at all; see docs/research for the survey behind issue #409.
	maxSharedFileListSerializedSize = 256 << 20
	// maxSharedFileListDecompressedSize bounds a *peer's* list on the decode
	// path, where a decompression bomb is a real risk to this process. It is
	// deliberately far stricter than the outbound limit above.
	//
	// The value is untested against real peers: slusk never sends a
	// SharedFileListRequest, so Deserialize has no production caller and this
	// cap has never actually run. Nicotine+ allows 2 GiB for the same message,
	// so 16 MiB would reject shares it reads without trouble - and, since the
	// outbound limit above is deliberately far larger, that now includes
	// slusk's own output: a share past roughly 170k files encodes to a frame
	// this decoder would refuse. Nothing reads it back today, but a second
	// slusk is not a peer we could browse.
	//
	// Revisit the number when browsing a peer becomes a feature - not before,
	// because until then a larger bound only buys a larger allocation for an
	// attacker to aim at.
	maxSharedFileListDecompressedSize = 16 << 20
	maxSharedFileListDirectories      = 100_000
	maxSharedFileListFiles            = 1_000_000
	maxSharedFileListAttributes       = 32
	maxSharedFileListStringSize       = 1 << 20
)

type sharedFileListLimitWriter struct {
	writer    io.Writer
	remaining int64
	limit     int64
	what      string
}

func newSharedFileListLimitWriter(w io.Writer, limit int64, what string) *sharedFileListLimitWriter {
	return &sharedFileListLimitWriter{writer: w, remaining: limit, limit: limit, what: what}
}

// Write reports the limit itself in the error, not just that one was hit: the
// only actionable thing a caller can say to a user whose share is too big is
// how big it was allowed to be (issue #408).
func (w *sharedFileListLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("%w: shared file list %s exceeds its %d-byte limit", soul.ErrMessageTooLarge, w.what, w.limit)
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

// SharedFileListResponse code 5 peer responds with a list of shared
// files after we’ve sent a SharedFileListRequest.
type SharedFileListResponse struct {
	Directories        []Directory
	PrivateDirectories []Directory
}

// Directory is a directory in a shared file list.
type Directory struct {
	Name  string
	Files []File
}

// File is a file in a directory.
type File struct {
	Name       string
	Size       uint64
	Extension  string
	Attributes []Attribute
}

// Attribute is a type of file attribute.
type Attribute struct {
	Code  FileAttributeType
	Value uint32
}

// Serialize accepts public and private directories. Empty share lists,
// directories, zero-byte files, and extensionless files are protocol-valid.
// Directory and file names themselves must remain nonempty.
func (s *SharedFileListResponse) Serialize(message *SharedFileListResponse) ([]byte, error) {
	if err := validateSharedFileList(message); err != nil {
		return nil, err
	}

	buf := new(bytes.Buffer)
	// Bounded by the shared constant directly rather than a local alias: this
	// value has to equal the session layer's write budget, and a second name
	// for it is a second thing to edit (issue #409).
	frameWriter := newSharedFileListLimitWriter(buf, MaxOrdinaryFrameSize, "frame")
	if err := internal.WriteUint32(frameWriter, uint32(CodeSharedFileListResponse)); err != nil {
		return nil, err
	}

	zw := zlib.NewWriter(frameWriter)
	payloadWriter := newSharedFileListLimitWriter(zw, maxSharedFileListSerializedSize, "serialized payload")
	if err := s.walkWrite(message.Directories, payloadWriter); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := internal.WriteUint32(payloadWriter, 0); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := s.walkWrite(message.PrivateDirectories, payloadWriter); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	return internal.Pack(buf.Bytes())
}

func validateSharedFileList(message *SharedFileListResponse) error {
	if len(message.Directories) > maxSharedFileListDirectories || len(message.PrivateDirectories) > maxSharedFileListDirectories-len(message.Directories) {
		return fmt.Errorf("%w: shared file list contains more than %d directories", soul.ErrMessageTooLarge, maxSharedFileListDirectories)
	}
	fileCount := 0
	for _, directories := range [][]Directory{message.Directories, message.PrivateDirectories} {
		for _, directory := range directories {
			if len(directory.Name) > maxSharedFileListStringSize {
				return fmt.Errorf("%w: shared file list directory name exceeds %d bytes", soul.ErrMessageTooLarge, maxSharedFileListStringSize)
			}
			if len(directory.Files) > maxSharedFileListFiles-fileCount {
				return fmt.Errorf("%w: shared file list contains more than %d files", soul.ErrMessageTooLarge, maxSharedFileListFiles)
			}
			fileCount += len(directory.Files)
			for _, file := range directory.Files {
				if len(file.Name) > maxSharedFileListStringSize || len(file.Extension) > maxSharedFileListStringSize {
					return fmt.Errorf("%w: shared file list file string exceeds %d bytes", soul.ErrMessageTooLarge, maxSharedFileListStringSize)
				}
				if len(file.Attributes) > maxSharedFileListAttributes {
					return fmt.Errorf("%w: shared file list file contains more than %d attributes", soul.ErrMessageTooLarge, maxSharedFileListAttributes)
				}
			}
		}
	}
	return nil
}

// ErrNoDirectories is returned when there are no directories.
var ErrNoDirectories = errors.New("no directories")

// ErrEmptyDirectoryName is returned when the directory name is empty.
var ErrEmptyDirectoryName = errors.New("directory name is empty")

// ErrEmptyDirectory is returned when the directory is empty.
var ErrEmptyDirectory = errors.New("directory is empty")

// ErrEmptyFileName is returned when the file name is empty.
var ErrEmptyFileName = errors.New("file name is empty")

// ErrSizeZero is returned when the file size is zero.
var ErrSizeZero = errors.New("file size is zero")

// ErrEmptyFileExtension is returned when the file extension is empty.
var ErrEmptyFileExtension = errors.New("file extension is empty")

func (SharedFileListResponse) walkWrite(directories []Directory, writer io.Writer) error {
	err := internal.WriteUint32(writer, uint32(len(directories)))
	if err != nil {
		return err
	}

	for _, directory := range directories {
		if directory.Name == "" {
			return ErrEmptyDirectoryName
		}

		err = internal.WriteString(writer, directory.Name)
		if err != nil {
			return err
		}

		err = internal.WriteUint32(writer, uint32(len(directory.Files)))
		if err != nil {
			return err
		}

		for _, file := range directory.Files {
			if file.Name == "" {
				return ErrEmptyFileName
			}

			err = internal.WriteUint8(writer, 1)
			if err != nil {
				return err
			}

			err = internal.WriteString(writer, file.Name)
			if err != nil {
				return err
			}

			err = internal.WriteUint64(writer, file.Size)
			if err != nil {
				return err
			}

			err = internal.WriteString(writer, file.Extension)
			if err != nil {
				return err
			}

			err = internal.WriteUint32(writer, uint32(len(file.Attributes)))
			if err != nil {
				return err
			}

			for _, attribute := range file.Attributes {
				err = internal.WriteUint32(writer, uint32(attribute.Code))
				if err != nil {
					return err
				}

				err = internal.WriteUint32(writer, attribute.Value)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// Deserialize accepts a reader and deserializes the message into the SharedFileListResponse struct.
func (s *SharedFileListResponse) Deserialize(reader io.Reader) error {
	if _, err := internal.ReadUint32(reader); err != nil { // size
		return err
	}

	code, err := internal.ReadUint32(reader)
	if err != nil {
		return err
	}
	if code != uint32(CodeSharedFileListResponse) {
		return errors.Join(soul.ErrMismatchingCodes,
			fmt.Errorf("expected code %d, got %d", CodeSharedFileListResponse, code))
	}

	decompressed, err := readBoundedBrowsePayload(reader, maxSharedFileListDecompressedSize, "shared file list response")
	if err != nil {
		return err
	}
	payload := bytes.NewReader(decompressed)
	var decoded SharedFileListResponse
	remainingDirectories := uint32(maxSharedFileListDirectories)
	remainingFiles := uint32(maxSharedFileListFiles)

	directories, err := internal.ReadUint32(payload)
	if err != nil {
		return err
	}
	decoded.Directories, err = readBrowseDirectories(payload, directories, &remainingDirectories, &remainingFiles, maxSharedFileListDirectories, maxSharedFileListFiles, maxSharedFileListAttributes, maxSharedFileListStringSize, "shared file list response")
	if err != nil {
		return err
	}

	if _, err = internal.ReadUint32(payload); err != nil { // unknown separator
		return err
	}
	privateDirectories, err := internal.ReadUint32(payload)
	if err != nil {
		return err
	}
	decoded.PrivateDirectories, err = readBrowseDirectories(payload, privateDirectories, &remainingDirectories, &remainingFiles, maxSharedFileListDirectories, maxSharedFileListFiles, maxSharedFileListAttributes, maxSharedFileListStringSize, "shared file list response")
	if err != nil {
		return err
	}

	*s = decoded
	return nil
}

func readBoundedBrowsePayload(reader io.Reader, limit int64, what string) ([]byte, error) {
	zr, err := zlib.NewReader(reader)
	if err != nil {
		return nil, err
	}
	decompressed, readErr := io.ReadAll(io.LimitReader(zr, limit+1))
	closeErr := zr.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(decompressed)) > limit {
		return nil, fmt.Errorf("%w: decompressed %s exceeds %d bytes", soul.ErrMessageTooLarge, what, limit)
	}
	return decompressed, nil
}

func readBrowseString(reader io.Reader, limit uint32, what string) (string, error) {
	size, err := internal.ReadStringLen(reader)
	if err != nil {
		return "", err
	}
	if size > limit {
		return "", fmt.Errorf("%w: %s string exceeds %d bytes", soul.ErrMessageTooLarge, what, limit)
	}
	return internal.ReadStringBody(reader, size)
}

func readBrowseDirectories(reader io.Reader, count uint32, remainingDirectories, remainingFiles *uint32, directoryLimit, fileLimit, attributeLimit, stringLimit uint32, what string) ([]Directory, error) {
	if count > *remainingDirectories {
		return nil, fmt.Errorf("%w: %s contains more than %d directories", soul.ErrMessageTooLarge, what, directoryLimit)
	}
	*remainingDirectories -= count
	var directories []Directory
	for i := uint32(0); i < count; i++ {
		name, err := readBrowseString(reader, stringLimit, what)
		if err != nil {
			return nil, err
		}
		fileCount, err := internal.ReadUint32(reader)
		if err != nil {
			return nil, err
		}
		if fileCount > *remainingFiles {
			return nil, fmt.Errorf("%w: %s contains more than %d files", soul.ErrMessageTooLarge, what, fileLimit)
		}
		*remainingFiles -= fileCount
		files, err := readBrowseFiles(reader, fileCount, attributeLimit, stringLimit, what)
		if err != nil {
			return nil, err
		}
		directories = append(directories, Directory{Name: name, Files: files})
	}
	return directories, nil
}

func readBrowseFiles(reader io.Reader, count, attributeLimit, stringLimit uint32, what string) ([]File, error) {
	var files []File
	for i := uint32(0); i < count; i++ {
		if _, err := internal.ReadUint8(reader); err != nil {
			return nil, err
		}
		name, err := readBrowseString(reader, stringLimit, what)
		if err != nil {
			return nil, err
		}
		size, err := internal.ReadUint64(reader)
		if err != nil {
			return nil, err
		}
		extension, err := readBrowseString(reader, stringLimit, what)
		if err != nil {
			return nil, err
		}
		attributeCount, err := internal.ReadUint32(reader)
		if err != nil {
			return nil, err
		}
		if attributeCount > attributeLimit {
			return nil, fmt.Errorf("%w: %s file contains more than %d attributes", soul.ErrMessageTooLarge, what, attributeLimit)
		}
		var attributes []Attribute
		for j := uint32(0); j < attributeCount; j++ {
			code, err := internal.ReadUint32(reader)
			if err != nil {
				return nil, err
			}
			value, err := internal.ReadUint32(reader)
			if err != nil {
				return nil, err
			}
			attributes = append(attributes, Attribute{Code: FileAttributeType(code), Value: value})
		}
		files = append(files, File{Name: name, Size: size, Extension: extension, Attributes: attributes})
	}
	return files, nil
}
