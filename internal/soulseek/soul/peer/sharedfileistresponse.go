package peer

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/internal"
)

const CodeSharedFileListResponse Code = 5

const (
	maxSharedFileListFrameSize        = 16 << 20 // declared size, excluding its 4-byte prefix
	maxSharedFileListDecompressedSize = 16 << 20
	maxSharedFileListDirectories      = 100_000
	maxSharedFileListFiles            = 1_000_000
	maxSharedFileListAttributes       = 32
	maxSharedFileListStringSize       = 1 << 20
)

type sharedFileListLimitWriter struct {
	writer    io.Writer
	remaining int64
	what      string
}

func (w *sharedFileListLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("%w: shared file list %s exceeds its configured limit", soul.ErrMessageTooLarge, w.what)
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
	frameWriter := &sharedFileListLimitWriter{writer: buf, remaining: maxSharedFileListFrameSize, what: "frame"}
	if err := internal.WriteUint32(frameWriter, uint32(CodeSharedFileListResponse)); err != nil {
		return nil, err
	}

	zw := zlib.NewWriter(frameWriter)
	payloadWriter := &sharedFileListLimitWriter{writer: zw, remaining: maxSharedFileListDecompressedSize, what: "decompressed payload"}
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
	_, err := internal.ReadUint32(reader) // size
	if err != nil {
		return err
	}

	code, err := internal.ReadUint32(reader) // code 5
	if err != nil {
		return err
	}

	if code != uint32(CodeSharedFileListResponse) {
		return errors.Join(soul.ErrMismatchingCodes,
			fmt.Errorf("expected code %d, got %d", CodeSharedFileListResponse, code))
	}

	zr, err := zlib.NewReader(reader)
	if err != nil {
		return err
	}

	defer zr.Close()

	directories, err := internal.ReadUint32(zr)
	if err != nil {
		return err
	}

	s.Directories, err = s.walkRead(directories, zr)
	if err != nil {
		return err
	}

	_, err = internal.ReadUint32(zr)
	if err != nil {
		return err
	}

	privateDirectories, err := internal.ReadUint32(zr)
	if err != nil {
		return err
	}

	s.PrivateDirectories, err = s.walkRead(privateDirectories, zr)
	if err != nil {
		return err
	}

	return nil
}

func (s *SharedFileListResponse) walkRead(numberOfDirectories uint32, zr io.ReadCloser) (directories []Directory, err error) {
	for range int(numberOfDirectories) {
		var directory Directory
		var err error

		directory.Name, err = internal.ReadString(zr)
		if err != nil {
			return nil, err
		}

		files, err := internal.ReadUint32(zr)
		if err != nil {
			return nil, err
		}

		for range int(files) {
			var f File

			_, err := internal.ReadUint8(zr)
			if err != nil {
				return nil, err
			}

			f.Name, err = internal.ReadString(zr)
			if err != nil {
				return nil, err
			}

			f.Size, err = internal.ReadUint64(zr)
			if err != nil {
				return nil, err
			}

			f.Extension, err = internal.ReadString(zr)
			if err != nil {
				return nil, err
			}

			attributes, err := internal.ReadUint32(zr)
			if err != nil {
				return nil, err
			}

			for range int(attributes) {
				var a Attribute

				code, err := internal.ReadUint32(zr)
				if err != nil {
					return nil, err
				}

				a.Code = FileAttributeType(code)

				a.Value, err = internal.ReadUint32(zr)
				if err != nil {
					return nil, err
				}

				f.Attributes = append(f.Attributes, a)
			}

			directory.Files = append(directory.Files, f)
		}

		directories = append(directories, directory)
	}

	return directories, err
}
