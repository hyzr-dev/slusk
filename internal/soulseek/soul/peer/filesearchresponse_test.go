package peer

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/internal"
)

func fileSearchResponseMandatoryPayload(t *testing.T) []byte {
	t.Helper()

	buf := new(bytes.Buffer)
	mustWriteString(t, buf, "searcher")
	mustWriteUint32(t, buf, 42) // token
	mustWriteUint32(t, buf, 0)  // public result count
	if err := internal.WriteBool(buf, true); err != nil {
		t.Fatalf("write free slot: %v", err)
	}
	mustWriteUint32(t, buf, 128_000) // average speed
	mustWriteUint32(t, buf, 3)       // queue
	return buf.Bytes()
}

func fileSearchResponseFrame(t *testing.T, payload []byte) []byte {
	t.Helper()

	buf := new(bytes.Buffer)
	mustWriteUint32(t, buf, uint32(CodeFileSearchResponse))
	zw := zlib.NewWriter(buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("compress payload: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close compressor: %v", err)
	}

	frame, err := internal.Pack(buf.Bytes())
	if err != nil {
		t.Fatalf("pack frame: %v", err)
	}
	return frame
}

func mustWriteUint32(t *testing.T, writer io.Writer, value uint32) {
	t.Helper()
	if err := internal.WriteUint32(writer, value); err != nil {
		t.Fatalf("write uint32: %v", err)
	}
}

func mustWriteString(t *testing.T, writer io.Writer, value string) {
	t.Helper()
	if err := internal.WriteString(writer, value); err != nil {
		t.Fatalf("write string: %v", err)
	}
}

func appendFileSearchResponseFile(t *testing.T, buf *bytes.Buffer, file File) {
	t.Helper()

	if err := internal.WriteUint8(buf, 1); err != nil {
		t.Fatalf("write file marker: %v", err)
	}
	mustWriteString(t, buf, file.Name)
	if err := internal.WriteUint64(buf, file.Size); err != nil {
		t.Fatalf("write file size: %v", err)
	}
	mustWriteString(t, buf, file.Extension)
	mustWriteUint32(t, buf, uint32(len(file.Attributes)))
	for _, attribute := range file.Attributes {
		mustWriteUint32(t, buf, uint32(attribute.Code))
		mustWriteUint32(t, buf, attribute.Value)
	}
}

func TestFileSearchResponseDeserializeTailLayouts(t *testing.T) {
	privateFile := File{
		Name:      `private\\album\\track.flac`,
		Size:      123456,
		Extension: "flac",
		Attributes: []Attribute{
			{Code: Bitrate, Value: 1411},
		},
	}

	tests := []struct {
		name        string
		appendTail  func(*bytes.Buffer)
		wantPrivate []File
	}{
		{
			name: "mandatory only",
		},
		{
			name: "unknown only without private list",
			appendTail: func(buf *bytes.Buffer) {
				mustWriteUint32(t, buf, 0)
			},
		},
		{
			name: "full tail",
			appendTail: func(buf *bytes.Buffer) {
				mustWriteUint32(t, buf, 0)
				mustWriteUint32(t, buf, 1)
				appendFileSearchResponseFile(t, buf, privateFile)
			},
			wantPrivate: []File{privateFile},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := bytes.NewBuffer(fileSearchResponseMandatoryPayload(t))
			if tt.appendTail != nil {
				tt.appendTail(payload)
			}

			var response FileSearchResponse
			if err := response.Deserialize(bytes.NewReader(fileSearchResponseFrame(t, payload.Bytes()))); err != nil {
				t.Fatalf("Deserialize: %v", err)
			}
			if response.Username != "searcher" || response.Token != 42 || !response.FreeSlot || response.AverageSpeed != 128_000 || response.Queue != 3 {
				t.Fatalf("mandatory fields = %+v", response)
			}
			if !reflect.DeepEqual(response.PrivateResults, tt.wantPrivate) {
				t.Fatalf("PrivateResults = %#v, want %#v", response.PrivateResults, tt.wantPrivate)
			}
		})
	}
}

func TestFileSearchResponseDeserializeRejectsPartialTail(t *testing.T) {
	tests := []struct {
		name       string
		appendTail func(*bytes.Buffer)
	}{
		{
			name: "partial unknown field",
			appendTail: func(buf *bytes.Buffer) {
				buf.Write([]byte{0, 0})
			},
		},
		{
			name: "partial private count",
			appendTail: func(buf *bytes.Buffer) {
				mustWriteUint32(t, buf, 0)
				buf.Write([]byte{1, 0})
			},
		},
		{
			name: "partial private file",
			appendTail: func(buf *bytes.Buffer) {
				mustWriteUint32(t, buf, 0)
				mustWriteUint32(t, buf, 1)
				if err := internal.WriteUint8(buf, 1); err != nil {
					t.Fatalf("write file marker: %v", err)
				}
				mustWriteUint32(t, buf, 8)
				buf.WriteString("short")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := bytes.NewBuffer(fileSearchResponseMandatoryPayload(t))
			tt.appendTail(payload)

			var response FileSearchResponse
			err := response.Deserialize(bytes.NewReader(fileSearchResponseFrame(t, payload.Bytes())))
			if err == nil {
				t.Fatal("Deserialize: expected truncation error")
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("Deserialize error = clean EOF, want hard truncation error: %v", err)
			}
		})
	}
}

func TestFileSearchResponseDeserializeValidatesZlibChecksum(t *testing.T) {
	frame := fileSearchResponseFrame(t, fileSearchResponseMandatoryPayload(t))
	frame[len(frame)-1] ^= 0xff

	var response FileSearchResponse
	if err := response.Deserialize(bytes.NewReader(frame)); err == nil {
		t.Fatal("Deserialize: expected corrupt zlib trailer error")
	}
}

func TestFileSearchResponseDeserializeRejectsExtraDecompressedData(t *testing.T) {
	payload := bytes.NewBuffer(fileSearchResponseMandatoryPayload(t))
	mustWriteUint32(t, payload, 0) // unknown
	mustWriteUint32(t, payload, 0) // private result count
	payload.WriteByte(0xff)

	var response FileSearchResponse
	err := response.Deserialize(bytes.NewReader(fileSearchResponseFrame(t, payload.Bytes())))
	if !errors.Is(err, errUnexpectedFileSearchResponseData) {
		t.Fatalf("Deserialize error = %v, want unexpected-data error", err)
	}
}

func TestFileSearchResponseDeserializeLimits(t *testing.T) {
	tests := []struct {
		name    string
		payload func() []byte
	}{
		{
			name: "public file count",
			payload: func() []byte {
				buf := new(bytes.Buffer)
				mustWriteString(t, buf, "searcher")
				mustWriteUint32(t, buf, 42)
				mustWriteUint32(t, buf, maxFileSearchResponseFiles+1)
				return buf.Bytes()
			},
		},
		{
			name: "private file count",
			payload: func() []byte {
				buf := bytes.NewBuffer(fileSearchResponseMandatoryPayload(t))
				mustWriteUint32(t, buf, 0)
				mustWriteUint32(t, buf, maxFileSearchResponseFiles+1)
				return buf.Bytes()
			},
		},
		{
			name: "attribute count",
			payload: func() []byte {
				buf := new(bytes.Buffer)
				mustWriteString(t, buf, "searcher")
				mustWriteUint32(t, buf, 42)
				mustWriteUint32(t, buf, 1)
				if err := internal.WriteUint8(buf, 1); err != nil {
					t.Fatalf("write marker: %v", err)
				}
				mustWriteString(t, buf, "track.mp3")
				if err := internal.WriteUint64(buf, 1); err != nil {
					t.Fatalf("write size: %v", err)
				}
				mustWriteString(t, buf, "mp3")
				mustWriteUint32(t, buf, maxFileSearchResponseAttributes+1)
				return buf.Bytes()
			},
		},
		{
			name: "string size",
			payload: func() []byte {
				buf := new(bytes.Buffer)
				mustWriteUint32(t, buf, maxFileSearchResponseStringSize+1)
				return buf.Bytes()
			},
		},
		{
			name: "decompressed data",
			payload: func() []byte {
				return make([]byte, maxFileSearchResponseDecompressedSize+1)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var response FileSearchResponse
			err := response.Deserialize(bytes.NewReader(fileSearchResponseFrame(t, tt.payload())))
			if !errors.Is(err, soul.ErrMessageTooLarge) {
				t.Fatalf("Deserialize error = %v, want ErrMessageTooLarge", err)
			}
		})
	}
}

func TestFileSearchResponseRoundTrip(t *testing.T) {
	want := FileSearchResponse{
		Username:     "searcher",
		Token:        987,
		FreeSlot:     true,
		AverageSpeed: 250_000,
		Queue:        2,
		Results: []File{
			{
				Name:      `music\\artist\\track.mp3`,
				Size:      654321,
				Extension: "mp3",
				Attributes: []Attribute{
					{Code: Bitrate, Value: 320},
					{Code: Duration, Value: 245},
				},
			},
		},
		PrivateResults: []File{
			{Name: `private\\track.flac`, Size: 1234567, Extension: "flac"},
		},
	}

	frame, err := want.Serialize(&want)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	var got FileSearchResponse
	if err := got.Deserialize(bytes.NewReader(frame)); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestFileSearchResponseFramedMessageLimitUnchanged(t *testing.T) {
	if internal.MaxMessageSize != 64<<20 {
		t.Fatalf("MaxMessageSize = %d, want 64 MiB", internal.MaxMessageSize)
	}
}

func TestFileSearchResponseCodeIsLittleEndian(t *testing.T) {
	frame := fileSearchResponseFrame(t, fileSearchResponseMandatoryPayload(t))
	if got := binary.LittleEndian.Uint32(frame[4:8]); got != uint32(CodeFileSearchResponse) {
		t.Fatalf("code = %d, want %d", got, CodeFileSearchResponse)
	}
}
