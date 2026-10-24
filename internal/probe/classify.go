package probe

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"path/filepath"
	"strings"
)

// Class says how a browser can play a file.
type Class string

// Playability classes.
const (
	// Direct: every browser can play the original file.
	Direct Class = "direct"
	// Conditional: some browsers play the original (HEVC, AV1, VP9); others
	// need a rendition.
	Conditional Class = "conditional"
	// Remux: the codecs are compatible but the container is not.
	Remux Class = "remux"
	// AudioOnly: the video is compatible, the audio must be encoded again.
	AudioOnly Class = "audio"
	// Transcode: the video must be encoded again.
	Transcode Class = "transcode"
)

// Containers that YAMP distinguishes.
const (
	ContainerMP4      = "mp4"
	ContainerMOV      = "mov"
	ContainerWebM     = "webm"
	ContainerMatroska = "matroska"
	ContainerOther    = "other"
)

// Analysis is the result of Classify.
type Analysis struct {
	Class     Class
	Container string
	// Mime is the MIME type of the original, or "" if it is not served.
	Mime string
	// Codecs is the RFC 6381 codecs parameter of the original file.
	Codecs string
	Video  *Stream
	Audio  *Stream
	HDR    bool
}

// ErrNoVideo is returned by Classify for files without a video stream.
var ErrNoVideo = errors.New("no video stream")

// ContainerOf returns the container of a probed file.
func ContainerOf(r *Result, path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case strings.Contains(r.Format.FormatName, "mp4"), strings.Contains(r.Format.FormatName, "mov"):
		if ext == ".mov" || strings.TrimSpace(r.Format.Tags["major_brand"]) == "qt" {
			return ContainerMOV
		}
		return ContainerMP4
	case strings.Contains(r.Format.FormatName, "matroska"):
		if ext == ".webm" {
			return ContainerWebM
		}
		return ContainerMatroska
	}
	return ContainerOther
}

// H264Compatible reports whether every browser can decode the video stream:
// 8-bit 4:2:0 progressive SDR H.264.
func H264Compatible(v *Stream) bool {
	if v == nil || v.CodecName != "h264" {
		return false
	}
	if v.PixFmt != "yuv420p" && v.PixFmt != "yuvj420p" {
		return false
	}
	return v.BitDepth() == 8 && !v.Interlaced() && !v.HDR()
}

// AACCompatible reports whether the audio can stay unchanged in MP4; no audio
// counts as compatible.
func AACCompatible(a *Stream) bool {
	return a == nil || a.CodecName == "aac"
}

func webmAudioOK(a *Stream) bool {
	return a == nil || a.CodecName == "opus" || a.CodecName == "vorbis"
}

// Classify decides how a browser can play the file at path. moovFirst reports
// whether the MP4 moov box comes before mdat (see MoovFirst).
func Classify(r *Result, path string, moovFirst bool) (Analysis, error) {
	a := Analysis{
		Container: ContainerOf(r, path),
		Video:     r.Video(),
		Audio:     r.Audio(""),
	}
	if a.Video == nil {
		return a, ErrNoVideo
	}
	a.HDR = a.Video.HDR()
	webm := a.Container == ContainerWebM
	a.Codecs = JoinCodecs(webm, a.Video, a.Audio)

	isMP4 := a.Container == ContainerMP4
	switch {
	case H264Compatible(a.Video):
		switch {
		case !AACCompatible(a.Audio):
			a.Class = AudioOnly
		case isMP4 && moovFirst:
			a.Class = Direct
		default:
			a.Class = Remux
		}
	case isMP4 && moovFirst && AACCompatible(a.Audio) && conditionalMP4Video(a.Video):
		a.Class = Conditional
	case webm && webmAudioOK(a.Audio) && (a.Video.CodecName == "vp9" || a.Video.CodecName == "av1" || a.Video.CodecName == "vp8"):
		a.Class = Conditional
	default:
		a.Class = Transcode
	}
	if a.Class == Direct || a.Class == Conditional {
		a.Mime = "video/mp4"
		if webm {
			a.Mime = "video/webm"
		}
	}
	return a, nil
}

func conditionalMP4Video(v *Stream) bool {
	switch v.CodecName {
	case "hevc":
		// With the hev1 tag Safari plays the audio but shows a black screen.
		return v.CodecTagString == "hvc1"
	case "av1", "vp9":
		return true
	}
	return false
}

// MoovFirst reports whether an MP4's moov box precedes mdat, so playback can
// start before the download ends. Non-MP4 files report false.
func MoovFirst(r io.ReaderAt, size int64) (bool, error) {
	var (
		off int64
		hdr [16]byte
	)
	for i := 0; i < 1024 && off+8 <= size; i++ {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return false, err
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		switch string(hdr[4:8]) {
		case "moov":
			return true, nil
		case "mdat":
			return false, nil
		}
		switch boxSize {
		case 0: // the box extends to the end of the file
			return false, nil
		case 1: // a 64-bit size follows the type
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return false, err
			}
			large := binary.BigEndian.Uint64(hdr[8:16])
			if large < 16 || large > math.MaxInt64 {
				return false, nil
			}
			boxSize = int64(large)
		default:
			if boxSize < 8 {
				return false, nil
			}
		}
		off += boxSize
	}
	return false, nil
}
