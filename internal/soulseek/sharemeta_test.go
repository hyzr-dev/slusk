package soulseek

import (
	"encoding/binary"
	"math"
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

// buildXingMP3 constructs a minimal MP3 buffer whose first (and only)
// frame carries a Xing/Info VBR header. tagOffset is the byte offset of the
// tag within the frame, i.e. 4 (frame header) plus the Layer III side info
// size for the given header's MPEG version/channel mode. If id3 is true, a
// small ID3v2 tag is prepended before the frame.
func buildXingMP3(header [4]byte, tagOffset int, tag string, frameCount uint32, id3 bool) []byte {
	frame := make([]byte, tagOffset+12)
	copy(frame[0:4], header[:])
	copy(frame[tagOffset:tagOffset+4], tag)
	binary.BigEndian.PutUint32(frame[tagOffset+4:tagOffset+8], 0x00000001) // Xing FRAMES flag
	binary.BigEndian.PutUint32(frame[tagOffset+8:tagOffset+12], frameCount)

	if !id3 {
		return frame
	}
	return append(buildID3Header(50), frame...)
}

// buildID3Header returns a minimal 10-byte ID3v2 header followed by
// tagSize bytes of tag body, with the syncsafe size field set accordingly.
func buildID3Header(tagSize int) []byte {
	h := make([]byte, 10+tagSize)
	copy(h[:3], "ID3")
	h[3], h[4] = 3, 0 // version 2.3.0
	h[6] = byte((tagSize >> 21) & 0x7f)
	h[7] = byte((tagSize >> 14) & 0x7f)
	h[8] = byte((tagSize >> 7) & 0x7f)
	h[9] = byte(tagSize & 0x7f)
	return h
}

func TestMP3DurationXing(t *testing.T) {
	const frameCount = 3829
	want := frameCount * 1152.0 / 44100

	cases := []struct {
		name      string
		header    [4]byte
		tagOffset int
		tag       string
	}{
		{"stereo Xing", [4]byte{0xff, 0xfb, 0x90, 0x00}, 36, "Xing"},
		{"stereo Info", [4]byte{0xff, 0xfb, 0x90, 0x00}, 36, "Info"},
		{"mono Xing", [4]byte{0xff, 0xfb, 0x90, 0xc0}, 21, "Xing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Keep the file itself tiny; a CBR estimate from its size would
			// be a small fraction of a second, proving the Xing frame count
			// takes precedence.
			data := buildXingMP3(tc.header, tc.tagOffset, tc.tag, frameCount, false)
			path := filepath.Join(t.TempDir(), "xing.mp3")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := mp3Duration(path)
			if err != nil {
				t.Fatalf("mp3Duration: %v", err)
			}
			if math.Abs(got-want) > 0.01 {
				t.Fatalf("duration = %v, want ~%v", got, want)
			}
		})
	}
}

func TestMP3DurationXingWithID3(t *testing.T) {
	const frameCount = 3829
	want := frameCount * 1152.0 / 44100
	header := [4]byte{0xff, 0xfb, 0x90, 0x00} // MPEG-1 stereo
	data := buildXingMP3(header, 36, "Xing", frameCount, true)
	path := filepath.Join(t.TempDir(), "xing_id3.mp3")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := mp3Duration(path)
	if err != nil {
		t.Fatalf("mp3Duration: %v", err)
	}
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("duration = %v, want ~%v", got, want)
	}

	// Without a Xing header, an ID3-prefixed CBR file must estimate duration
	// from (fileSize - id3TagBytes), not the raw file size.
	frame := make([]byte, 417)
	copy(frame, []byte{0xff, 0xfb, 0x90, 0x00})
	const frames = 40
	cbr := make([]byte, 0, len(frame)*frames)
	for range frames {
		cbr = append(cbr, frame...)
	}
	full := append(buildID3Header(10000), cbr...)
	cbrPath := filepath.Join(t.TempDir(), "cbr_id3.mp3")
	if err := os.WriteFile(cbrPath, full, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = mp3Duration(cbrPath)
	if err != nil {
		t.Fatalf("mp3Duration: %v", err)
	}
	wantCBR := float64(len(cbr)) * 8 / (128 * 1000)
	if math.Abs(got-wantCBR) > 1e-3 {
		t.Fatalf("CBR+ID3 duration = %v, want ~%v", got, wantCBR)
	}
}

func TestMP3DurationDoesNotReadPastCap(t *testing.T) {
	// Behavioral: build ~1MB of valid CBR frames, but overlay a Xing header
	// on the first frame claiming a small frame count. The new
	// implementation must trust the Xing count (a few seconds) rather than
	// estimate from the (uncapped) frame-sum/size (~65s).
	const smallFrameCount = 100
	want := smallFrameCount * 1152.0 / 44100

	frame := make([]byte, 417)
	copy(frame, []byte{0xff, 0xfb, 0x90, 0x00})
	const frames = 2500 // ~1MB total
	data := make([]byte, 0, len(frame)*frames)
	for range frames {
		data = append(data, frame...)
	}
	// Stereo MPEG-1 Layer III -> Xing tag at offset 4+32=36 within the frame.
	copy(data[36:40], "Xing")
	binary.BigEndian.PutUint32(data[40:44], 0x00000001)
	binary.BigEndian.PutUint32(data[44:48], smallFrameCount)

	path := filepath.Join(t.TempDir(), "capped.mp3")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := mp3Duration(path)
	if err != nil {
		t.Fatalf("mp3Duration: %v", err)
	}
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("duration = %v, want ~%v (Xing count should win over frame-sum/size estimate)", got, want)
	}

	// Structural: mp3Duration never reads more than 64KB of audio data by
	// construction. Verify estimateMP3Duration's CBR fallback still uses
	// the full audioSize passed in (not len(head)) when no Xing header is
	// present in that bounded head.
	plain := make([]byte, 0, len(frame)*frames)
	for range frames {
		plain = append(plain, frame...)
	}
	head := plain[:64*1024]
	gotSize, err := estimateMP3Duration(head, int64(len(plain)))
	if err != nil {
		t.Fatalf("estimateMP3Duration: %v", err)
	}
	wantSize := float64(len(plain)) * 8 / (128 * 1000)
	if math.Abs(gotSize-wantSize) > 0.01 {
		t.Fatalf("size-based estimate = %v, want ~%v", gotSize, wantSize)
	}
}

func assertAudioAttrs(t *testing.T, attrs []peer.Attribute, duration uint32) {
	t.Helper()
	if len(attrs) != 2 || attrs[0].Code != peer.Bitrate || attrs[0].Value == 0 || attrs[1] != (peer.Attribute{Code: peer.Duration, Value: duration}) {
		t.Fatalf("attributes = %#v", attrs)
	}
}
