package soulseek

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul/peer"
)

// ShareFileMeta is one file's cached technical metadata, keyed by its local
// path. Bitrate and Duration are both zero exactly when the file was examined
// and yielded no attributes at all — extractTechnicalMetadata rejects a zero
// bitrate or duration, so zero can never be a legitimate value and doubles as a
// cached negative result rather than "unknown".
type ShareFileMeta struct {
	Path     string
	Size     int64
	ModTime  time.Time
	Bitrate  uint32
	Duration uint32
}

// ShareMetaCache persists the result of reading a shared audio file's technical
// metadata so a restart does not have to open every mp3 and flac again. It is
// deliberately set-oriented rather than per-file: a round trip per file would
// trade filesystem latency for database latency at the same order of magnitude.
//
// Every method is best-effort. An error from either one is logged and the scan
// continues by reading the files — the cache may never affect what is
// advertised, only how long producing it takes.
type ShareMetaCache interface {
	// LoadShareMeta returns every cached row. The caller treats an error as an
	// empty cache.
	LoadShareMeta(ctx context.Context) ([]ShareFileMeta, error)

	// SaveShareMeta upserts every entry in upserts (keyed on Path) and deletes
	// the rows named by stalePaths. It is called only after a share scan has
	// walked every configured root successfully; stalePaths is exactly the set
	// of paths this scan loaded but did not observe.
	SaveShareMeta(ctx context.Context, upserts []ShareFileMeta, stalePaths []string) error
}

// audioFormatOf reports the audio format extractTechnicalMetadata recognizes
// for path ("mp3" or "flac"), or "" for anything else. Shared by
// extractTechnicalMetadata and scanShares's cache lookup so both agree on
// exactly which files are worth caching at all.
func audioFormatOf(path string) string {
	switch extensionOf(path) {
	case "flac":
		return "flac"
	case "mp3":
		return "mp3"
	default:
		return ""
	}
}

// attributesFromCache rebuilds the wire attributes extractTechnicalMetadata
// would have produced, from a cached entry. nil when both fields are zero —
// the cached negative result (see ShareFileMeta).
func attributesFromCache(m ShareFileMeta) []peer.Attribute {
	if m.Bitrate == 0 && m.Duration == 0 {
		return nil
	}
	return []peer.Attribute{{Code: peer.Bitrate, Value: m.Bitrate}, {Code: peer.Duration, Value: m.Duration}}
}

// attributeValues extracts the bitrate/duration values extractTechnicalMetadata
// placed on the wire (0/0 if attrs is nil, i.e. a negative result), for caching.
func attributeValues(attrs []peer.Attribute) (bitrate, duration uint32) {
	for _, a := range attrs {
		switch a.Code {
		case peer.Bitrate:
			bitrate = a.Value
		case peer.Duration:
			duration = a.Value
		}
	}
	return bitrate, duration
}

// sameShareFileMeta reports whether a cached entry is still valid for a file
// currently observed with size and mod. Compared on UnixMicro rather than
// time.Equal so a mod time that lost sub-microsecond precision on its way
// through the store (see migrations/0007_share_metadata_cache.sql) still
// compares equal - that is the whole point of storing microseconds.
func sameShareFileMeta(cached ShareFileMeta, size int64, mod time.Time) bool {
	return cached.Size == size && cached.ModTime.UnixMicro() == mod.UnixMicro()
}

func extractTechnicalMetadata(path string, size int64, logger *slog.Logger) []peer.Attribute {
	format := audioFormatOf(path)
	var seconds float64
	var err error
	switch format {
	case "flac":
		seconds, err = flacDuration(path)
	case "mp3":
		seconds, err = mp3Duration(path)
	default:
		return nil
	}
	if err != nil || seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		if err != nil && logger != nil {
			logger.Debug("audio metadata unavailable; sharing file without technical attributes", "path", path, "format", format, "err", err)
		}
		return nil
	}
	duration := uint64(seconds)
	bitrate := uint64(math.Round(float64(size) * 8 / seconds / 1000))
	if duration == 0 || duration > math.MaxUint32 || bitrate == 0 || bitrate > math.MaxUint32 {
		return nil
	}
	return []peer.Attribute{{Code: peer.Bitrate, Value: uint32(bitrate)}, {Code: peer.Duration, Value: uint32(duration)}}
}

func flacDuration(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := skipID3v2(f); err != nil {
		return 0, err
	}
	var signature [4]byte
	if _, err := io.ReadFull(f, signature[:]); err != nil {
		return 0, err
	}
	if string(signature[:]) != "fLaC" {
		return 0, errors.New("invalid FLAC signature")
	}
	for {
		var header [4]byte
		if _, err := io.ReadFull(f, header[:]); err != nil {
			return 0, err
		}
		blockType := header[0] & 0x7f
		last := header[0]&0x80 != 0
		length := int64(header[1])<<16 | int64(header[2])<<8 | int64(header[3])
		if blockType == 0 {
			if length != 34 {
				return 0, errors.New("invalid FLAC STREAMINFO length")
			}
			var data [34]byte
			if _, err := io.ReadFull(f, data[:]); err != nil {
				return 0, err
			}
			packed := binary.BigEndian.Uint64(data[10:18])
			sampleRate := (packed >> 44) & 0xfffff
			totalSamples := packed & 0xfffffffff
			if sampleRate == 0 || totalSamples == 0 {
				return 0, errors.New("FLAC has zero sample rate or samples")
			}
			return float64(totalSamples) / float64(sampleRate), nil
		}
		if _, err := f.Seek(length, io.SeekCurrent); err != nil {
			return 0, err
		}
		if last {
			return 0, errors.New("FLAC STREAMINFO missing")
		}
	}
}

// skipID3v2 positions f just past a leading ID3v2 tag, or at offset 0 if none.
func skipID3v2(f *os.File) error {
	var head [10]byte
	if _, err := io.ReadFull(f, head[:]); err != nil || string(head[:3]) != "ID3" {
		_, serr := f.Seek(0, io.SeekStart)
		return serr // a too-short file surfaces as a signature error in the caller
	}
	size := int64(head[6]&0x7f)<<21 | int64(head[7]&0x7f)<<14 | int64(head[8]&0x7f)<<7 | int64(head[9]&0x7f)
	_, err := f.Seek(10+size, io.SeekStart)
	return err
}

// mp3Duration estimates the playback duration of an MP3 file without
// decoding it fully. It reads at most the first 64 KB of audio data: if
// that data carries a Xing/Info VBR header, the exact frame count gives an
// exact duration; otherwise the first frame's bitrate is used to estimate
// duration from the audio data size (accurate for CBR files).
func mp3Duration(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	fileSize := info.Size()

	var id3Header [10]byte
	n, readErr := io.ReadFull(f, id3Header[:])
	var audioSize int64
	if readErr == nil && n == 10 && string(id3Header[:3]) == "ID3" {
		tagSize := int64(id3Header[6]&0x7f)<<21 | int64(id3Header[7]&0x7f)<<14 | int64(id3Header[8]&0x7f)<<7 | int64(id3Header[9]&0x7f)
		if _, err := f.Seek(10+tagSize, io.SeekStart); err != nil {
			return 0, err
		}
		audioSize = fileSize - (10 + tagSize)
	} else {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		audioSize = fileSize
	}

	head := make([]byte, 64*1024)
	n, err = io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return 0, err
	}
	head = head[:n]

	return estimateMP3Duration(head, audioSize)
}

// estimateMP3Duration derives a duration estimate from the first chunk of
// MPEG audio data (head) and the total size of the audio data (audioSize,
// which may exceed len(head)). It prefers the exact frame count from a
// Xing/Info VBR header when present, falling back to a CBR estimate based
// on the first frame's bitrate.
func estimateMP3Duration(head []byte, audioSize int64) (float64, error) {
	syncOff := -1
	var h uint32
	var samples, sampleRate, kbps int
	for i := 0; i+4 <= len(head); i++ {
		candidate := binary.BigEndian.Uint32(head[i : i+4])
		_, s, sr, kb, ok := parseMP3Header(candidate)
		if ok {
			syncOff = i
			h = candidate
			samples, sampleRate, kbps = s, sr, kb
			break
		}
	}
	if syncOff < 0 {
		return 0, errors.New("no valid MPEG audio frames")
	}

	// Xing/Info VBR header check. The tag sits right after the side info
	// that follows the 4-byte frame header; the CRC bit is ignored for
	// this placement, matching LAME/common encoder practice.
	versionBits := (h >> 19) & 3
	mpeg1 := versionBits == 3
	mono := (h>>6)&3 == 3
	var sideInfoSize int
	switch {
	case mpeg1 && mono:
		sideInfoSize = 17
	case mpeg1 && !mono:
		sideInfoSize = 32
	case !mpeg1 && mono:
		sideInfoSize = 9
	default: // MPEG-2/2.5, stereo
		sideInfoSize = 17
	}
	tagOffset := syncOff + 4 + sideInfoSize
	if tagOffset+4 <= len(head) {
		tag := string(head[tagOffset : tagOffset+4])
		if tag == "Xing" || tag == "Info" {
			flagsOffset := tagOffset + 4
			if flagsOffset+4 <= len(head) {
				flags := binary.BigEndian.Uint32(head[flagsOffset : flagsOffset+4])
				if flags&0x0001 != 0 { // FRAMES flag
					frameCountOffset := flagsOffset + 4
					if frameCountOffset+4 <= len(head) {
						frameCount := binary.BigEndian.Uint32(head[frameCountOffset : frameCountOffset+4])
						if frameCount > 0 && sampleRate > 0 {
							return float64(frameCount) * float64(samples) / float64(sampleRate), nil
						}
					}
				}
			}
		}
	}

	// CBR fallback: estimate from audio data size and the first frame's bitrate.
	if kbps <= 0 {
		return 0, errors.New("no valid MPEG audio frames")
	}
	duration := float64(audioSize-int64(syncOff)) * 8 / float64(kbps*1000)
	if duration <= 0 {
		return 0, errors.New("no valid MPEG audio frames")
	}
	return duration, nil
}

func parseMP3Header(h uint32) (frameLen, samples, sampleRate, kbps int, ok bool) {
	if h&0xffe00000 != 0xffe00000 {
		return 0, 0, 0, 0, false
	}
	versionBits := (h >> 19) & 3
	layerBits := (h >> 17) & 3
	bitrateIndex := (h >> 12) & 0xf
	rateIndex := (h >> 10) & 3
	padding := int((h >> 9) & 1)
	if versionBits == 1 || layerBits != 1 || bitrateIndex == 0 || bitrateIndex == 15 || rateIndex == 3 {
		return 0, 0, 0, 0, false // only Layer III, all MPEG versions
	}
	rates := [...]int{44100, 48000, 32000}
	sampleRate = rates[rateIndex]
	mpeg1 := versionBits == 3
	if versionBits == 2 {
		sampleRate /= 2
	} else if versionBits == 0 {
		sampleRate /= 4
	}
	if mpeg1 {
		kbps = [...]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}[bitrateIndex]
		samples = 1152
		frameLen = 144000*kbps/sampleRate + padding
	} else {
		kbps = [...]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}[bitrateIndex]
		samples = 576
		frameLen = 72000*kbps/sampleRate + padding
	}
	return frameLen, samples, sampleRate, kbps, frameLen >= 4
}
