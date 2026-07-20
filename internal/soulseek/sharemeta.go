package soulseek

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

func extractTechnicalMetadata(path string, size int64, logger *slog.Logger) []peer.Attribute {
	ext := extensionOf(path)
	var seconds float64
	var err error
	switch ext {
	case "flac":
		seconds, err = flacDuration(path)
	case "mp3":
		seconds, err = mp3Duration(path)
	default:
		return nil
	}
	if err != nil || seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		if err != nil && logger != nil {
			logger.Debug("audio metadata unavailable; sharing file without technical attributes", "path", path, "format", ext, "err", err)
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

func mp3Duration(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 32*1024)
	if head, _ := r.Peek(10); len(head) == 10 && string(head[:3]) == "ID3" {
		size := int64(head[6]&0x7f)<<21 | int64(head[7]&0x7f)<<14 | int64(head[8]&0x7f)<<7 | int64(head[9]&0x7f)
		if _, err := f.Seek(10+size, io.SeekStart); err != nil {
			return 0, err
		}
		r.Reset(f)
	}
	var total float64
	frames := 0
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, err
	}
	for {
		frameLen, samples, sampleRate, ok := parseMP3Header(binary.BigEndian.Uint32(header[:]))
		if !ok {
			if frames != 0 {
				break
			}
			next, err := r.ReadByte()
			if err != nil {
				break
			}
			header[0], header[1], header[2], header[3] = header[1], header[2], header[3], next
			continue
		}
		if _, err := io.CopyN(io.Discard, r, int64(frameLen-4)); err != nil {
			break
		}
		total += float64(samples) / float64(sampleRate)
		frames++
		if _, err := io.ReadFull(r, header[:]); err != nil {
			break
		}
	}
	if frames == 0 || total <= 0 {
		return 0, errors.New("no valid MPEG audio frames")
	}
	return total, nil
}

func parseMP3Header(h uint32) (frameLen, samples, sampleRate int, ok bool) {
	if h&0xffe00000 != 0xffe00000 {
		return 0, 0, 0, false
	}
	versionBits := (h >> 19) & 3
	layerBits := (h >> 17) & 3
	bitrateIndex := (h >> 12) & 0xf
	rateIndex := (h >> 10) & 3
	padding := int((h >> 9) & 1)
	if versionBits == 1 || layerBits != 1 || bitrateIndex == 0 || bitrateIndex == 15 || rateIndex == 3 {
		return 0, 0, 0, false // only Layer III, all MPEG versions
	}
	rates := [...]int{44100, 48000, 32000}
	sampleRate = rates[rateIndex]
	mpeg1 := versionBits == 3
	if versionBits == 2 {
		sampleRate /= 2
	} else if versionBits == 0 {
		sampleRate /= 4
	}
	var kbps int
	if mpeg1 {
		kbps = [...]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}[bitrateIndex]
		samples = 1152
		frameLen = 144000*kbps/sampleRate + padding
	} else {
		kbps = [...]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}[bitrateIndex]
		samples = 576
		frameLen = 72000*kbps/sampleRate + padding
	}
	return frameLen, samples, sampleRate, frameLen >= 4
}
