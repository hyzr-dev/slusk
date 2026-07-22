package soulseek

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

// flacBytes returns a synthetic minimal FLAC file: signature, a single
// last STREAMINFO block (44.1kHz, stereo, 16-bit, 2s of samples), and
// padding to make the file large enough for a plausible bitrate.
func flacBytes() []byte {
	data := make([]byte, 4+4+34+1000)
	copy(data, "fLaC")
	data[4] = 0x80 // last STREAMINFO block
	data[7] = 34
	packed := uint64(44100)<<44 | uint64(2-1)<<41 | uint64(16-1)<<36 | uint64(88200)
	binary.BigEndian.PutUint64(data[18:26], packed)
	return data
}

func TestFLACTechnicalMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.flac")
	data := flacBytes()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	attrs := extractTechnicalMetadata(path, int64(len(data)), nil)
	assertAudioAttrs(t, attrs, 2)
}

func TestFLACTechnicalMetadataWithID3v2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tagged.flac")
	id3 := make([]byte, 10+128)
	copy(id3, "ID3")
	// synchsafe size 128 -> id3[8] = 0x01, id3[9] = 0x00
	id3[8] = 0x01
	id3[9] = 0x00
	data := append(id3, flacBytes()...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	attrs := extractTechnicalMetadata(path, int64(len(data)), nil)
	assertAudioAttrs(t, attrs, 2)
}

func TestFLACTechnicalMetadataTruncatedOrGarbageID3v2(t *testing.T) {
	cases := map[string][]byte{
		"id3-only": []byte("ID3"),
		"id3-header-no-data": func() []byte {
			head := make([]byte, 10)
			copy(head, "ID3")
			// synchsafe size 1000, but no data follows
			head[6] = byte((1000 >> 21) & 0x7f)
			head[7] = byte((1000 >> 14) & 0x7f)
			head[8] = byte((1000 >> 7) & 0x7f)
			head[9] = byte(1000 & 0x7f)
			return head
		}(),
		"id3-header-then-garbage": func() []byte {
			id3 := make([]byte, 10+128)
			copy(id3, "ID3")
			id3[8] = 0x01
			id3[9] = 0x00
			return append(id3, []byte{0x00, 0x01, 0x02}...)
		}(),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.flac")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if attrs := extractTechnicalMetadata(path, int64(len(data)), nil); len(attrs) != 0 {
				t.Fatalf("malformed metadata attributes = %#v", attrs)
			}
		})
	}
}

func TestMP3TechnicalMetadataAndMalformedFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.mp3")
	// MPEG-1 Layer III, 128kbps, 44.1kHz: 417-byte frames. Forty frames
	// provide just over one second of technical duration.
	frame := make([]byte, 417)
	copy(frame, []byte{0xff, 0xfb, 0x90, 0x00})
	data := make([]byte, 0, len(frame)*40)
	for range 40 {
		data = append(data, frame...)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	attrs := extractTechnicalMetadata(path, int64(len(data)), nil)
	assertAudioAttrs(t, attrs, 1)

	bad := filepath.Join(t.TempDir(), "bad.flac")
	if err := os.WriteFile(bad, []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if attrs := extractTechnicalMetadata(bad, 6, nil); len(attrs) != 0 {
		t.Fatalf("malformed metadata attributes = %#v", attrs)
	}
}

func assertAudioAttrs(t *testing.T, attrs []peer.Attribute, duration uint32) {
	t.Helper()
	if len(attrs) != 2 || attrs[0].Code != peer.Bitrate || attrs[0].Value == 0 || attrs[1] != (peer.Attribute{Code: peer.Duration, Value: duration}) {
		t.Fatalf("attributes = %#v", attrs)
	}
}
