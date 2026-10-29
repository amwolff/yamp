// Package probe runs ffprobe, derives RFC 6381 codecs strings and classifies
// how a browser can play a file.
package probe

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Result is the subset of ffprobe's JSON output that YAMP uses.
type Result struct {
	Streams []Stream `json:"streams"`
	Format  Format   `json:"format"`
}

// Format describes the container.
type Format struct {
	FormatName string            `json:"format_name"`
	Duration   string            `json:"duration"`
	Size       string            `json:"size"`
	BitRate    string            `json:"bit_rate"`
	Tags       map[string]string `json:"tags"`
}

// Stream describes one elementary stream.
type Stream struct {
	Index            int               `json:"index"`
	CodecName        string            `json:"codec_name"`
	CodecType        string            `json:"codec_type"`
	CodecTagString   string            `json:"codec_tag_string"`
	Profile          string            `json:"profile"`
	Level            int               `json:"level"`
	PixFmt           string            `json:"pix_fmt"`
	Width            int               `json:"width"`
	Height           int               `json:"height"`
	FieldOrder       string            `json:"field_order"`
	ColorTransfer    string            `json:"color_transfer"`
	ColorPrimaries   string            `json:"color_primaries"`
	Channels         int               `json:"channels"`
	BitRate          string            `json:"bit_rate"`
	BitsPerRawSample string            `json:"bits_per_raw_sample"`
	RFrameRate       string            `json:"r_frame_rate"`
	AvgFrameRate     string            `json:"avg_frame_rate"`
	ExtradataDump    string            `json:"extradata"`
	Disposition      map[string]int    `json:"disposition"`
	Tags             map[string]string `json:"tags"`
	SideData         []SideData        `json:"side_data_list"`
}

// SideData is one side data entry of a stream.
type SideData struct {
	Type     string  `json:"side_data_type"`
	Rotation float64 `json:"rotation"`
}

// Prober runs ffprobe.
type Prober struct {
	FFprobe string
}

// Probe runs ffprobe on path. It returns the parsed result and the raw JSON.
func (p *Prober) Probe(ctx context.Context, path string) (*Result, []byte, error) {
	//nolint:gosec // G204: the configured ffprobe binary; YAMP builds the arguments and uses no shell.
	cmd := exec.CommandContext(ctx, p.FFprobe,
		"-v", "error", "-of", "json", "-show_format", "-show_streams", "-show_data", "--", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("ffprobe: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	r, err := Parse(out)
	if err != nil {
		return nil, nil, err
	}
	return r, out, nil
}

// Parse decodes ffprobe JSON output.
func Parse(data []byte) (*Result, error) {
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode ffprobe output: %w", err)
	}
	return &r, nil
}

// Duration returns the container duration.
func (r *Result) Duration() time.Duration {
	f, err := strconv.ParseFloat(r.Format.Duration, 64)
	if err != nil || f < 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

// BitRate returns the container bit rate in bits per second, or 0.
func (r *Result) BitRate() int64 {
	n, _ := strconv.ParseInt(r.Format.BitRate, 10, 64)
	return n
}

// Video returns the main video stream (skipping cover art), or nil.
func (r *Result) Video() *Stream {
	var first *Stream
	for i := range r.Streams {
		s := &r.Streams[i]
		if s.CodecType != "video" || s.Disposition["attached_pic"] == 1 {
			continue
		}
		if s.Disposition["default"] == 1 {
			return s
		}
		if first == nil {
			first = s
		}
	}
	return first
}

// Audio returns the first stream in preferLang (ISO 639-2, e.g. "eng"), else
// the default stream, else the first one; nil if there is no audio.
func (r *Result) Audio(preferLang string) *Stream {
	var first, def *Stream
	for i := range r.Streams {
		s := &r.Streams[i]
		if s.CodecType != "audio" {
			continue
		}
		if preferLang != "" && strings.EqualFold(s.Lang(), preferLang) {
			return s
		}
		if def == nil && s.Disposition["default"] == 1 {
			def = s
		}
		if first == nil {
			first = s
		}
	}
	if def != nil {
		return def
	}
	return first
}

var textSubtitleCodecs = map[string]bool{
	"subrip": true, "ass": true, "ssa": true, "mov_text": true, "webvtt": true, "text": true,
}

// TextSubtitles returns the subtitle streams that can be converted to WebVTT.
func (r *Result) TextSubtitles() []Stream {
	var subs []Stream
	for _, s := range r.Streams {
		if s.CodecType == "subtitle" && textSubtitleCodecs[s.CodecName] {
			subs = append(subs, s)
		}
	}
	return subs
}

// Lang returns the stream's language tag, or "".
func (s *Stream) Lang() string {
	if l := s.Tags["language"]; l != "und" {
		return l
	}
	return ""
}

// Title returns the stream's title tag, or "".
func (s *Stream) Title() string { return s.Tags["title"] }

// FPS returns the average frame rate, or the real base frame rate if the
// average is unknown.
func (s *Stream) FPS() float64 {
	if f := parseRational(s.AvgFrameRate); f > 0 {
		return f
	}
	return parseRational(s.RFrameRate)
}

// StreamBitRate returns the stream bit rate in bits per second, or 0.
func (s *Stream) StreamBitRate() int64 {
	n, _ := strconv.ParseInt(s.BitRate, 10, 64)
	return n
}

// BitDepth returns the luma bit depth of a video stream.
func (s *Stream) BitDepth() int {
	switch {
	case strings.Contains(s.PixFmt, "p10"), strings.Contains(s.PixFmt, "p010"):
		return 10
	case strings.Contains(s.PixFmt, "p12"):
		return 12
	}
	if n, err := strconv.Atoi(s.BitsPerRawSample); err == nil && n > 0 {
		return n
	}
	return 8
}

// Chroma420 reports whether a video stream uses 4:2:0 chroma subsampling.
func (s *Stream) Chroma420() bool {
	return strings.Contains(s.PixFmt, "420")
}

// HDR reports whether the stream uses a PQ or HLG transfer function.
func (s *Stream) HDR() bool {
	return s.ColorTransfer == "smpte2084" || s.ColorTransfer == "arib-std-b67"
}

// Interlaced reports whether the stream is interlaced.
func (s *Stream) Interlaced() bool {
	switch s.FieldOrder {
	case "tt", "bb", "tb", "bt":
		return true
	}
	return false
}

// DisplaySize returns the width and height after the rotation in the display
// matrix. ffmpeg applies this rotation before the filters.
func (s *Stream) DisplaySize() (int, int) {
	for _, sd := range s.SideData {
		if sd.Type == "Display Matrix" && int(math.Abs(math.Round(sd.Rotation)))%180 == 90 {
			return s.Height, s.Width
		}
	}
	return s.Width, s.Height
}

// Extradata decodes the hexdump that ffprobe prints with -show_data. Each line
// looks like "00000000: 0164 0028 ffe1 ...  .d.(..". It returns nil if the
// stream has no extradata.
func (s *Stream) Extradata() []byte {
	var out []byte
	for _, line := range strings.Split(s.ExtradataDump, "\n") {
		_, rest, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		// Eight groups of four hex digits and seven separators are followed by
		// two spaces and the ASCII column.
		if len(rest) > 39 {
			rest = rest[:39]
		}
		b, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(rest), " ", ""))
		if err != nil {
			return nil
		}
		out = append(out, b...)
	}
	return out
}

func parseRational(v string) float64 {
	num, den, ok := strings.Cut(v, "/")
	if !ok {
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}
