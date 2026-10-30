// Package transcode builds ffmpeg commands from transcode profiles, detects the
// available encoders and runs the job queue.
package transcode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// ArgBuilderVersion changes when BuildArgs changes its output for the same
// parameters. It is part of the parameter hash, so a new version marks the
// existing renditions stale.
const ArgBuilderVersion = 1

// Video modes.
const (
	VideoCopyIfCompatible = "copy-if-compatible"
	VideoEncode           = "encode"
)

// Auto-apply policies.
const (
	PolicyManual     = "manual"
	PolicyWhenNeeded = "when-needed"
	PolicyAlways     = "always"
)

// Policies lists the auto-apply policies.
var Policies = []string{PolicyManual, PolicyWhenNeeded, PolicyAlways}

// Codecs lists the target video codecs.
var Codecs = []string{"h264", "hevc", "av1"}

// EncoderCodec maps each supported encoder to the codec it produces.
var EncoderCodec = map[string]string{
	"libx264": "h264", "h264_qsv": "h264", "h264_vaapi": "h264",
	"libx265": "hevc", "hevc_qsv": "hevc", "hevc_vaapi": "hevc",
	"libsvtav1": "av1",
}

// autoOrder is the preference order of the encoder "auto": hardware first.
var autoOrder = map[string][]string{
	"h264": {"h264_qsv", "h264_vaapi", "libx264"},
	"hevc": {"hevc_qsv", "hevc_vaapi", "libx265"},
	"av1":  {"libsvtav1"},
}

// x264Presets are the presets of libx264 and libx265; QSV accepts the subset
// from veryfast to veryslow.
var x264Presets = []string{"ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow"}

// Params are the tunable parameters of a profile. The JSON form is canonical:
// the field order is fixed, so equal parameters give equal hashes.
type Params struct {
	VideoMode     string `json:"video_mode"`
	Codec         string `json:"codec"`
	Encoder       string `json:"encoder"`
	Quality       int    `json:"quality"`
	MaxBitrateK   int    `json:"max_bitrate_k"`
	Preset        string `json:"preset"`
	MaxHeight     int    `json:"max_height"`
	MaxFPS        int    `json:"max_fps"`
	TenBit        bool   `json:"ten_bit"`
	Tonemap       bool   `json:"tonemap"`
	AudioBitrateK int    `json:"audio_bitrate_k"`
	Stereo        bool   `json:"stereo"`
	AudioLang     string `json:"audio_lang"`
}

// Hash identifies the parameters together with the argument builder version.
func (p Params) Hash() string {
	b, _ := json.Marshal(p)
	h := sha256.New()
	h.Write(b)
	h.Write([]byte(strconv.Itoa(ArgBuilderVersion)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// JSON returns the canonical JSON form.
func (p Params) JSON() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// ParseParams decodes and validates the JSON form.
func ParseParams(s string) (Params, error) {
	var p Params
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}

// Validate checks the parameters.
func (p Params) Validate() error {
	var errs []error
	if p.VideoMode != VideoCopyIfCompatible && p.VideoMode != VideoEncode {
		errs = append(errs, fmt.Errorf("unknown video mode %q", p.VideoMode))
	}
	if !slices.Contains(Codecs, p.Codec) {
		errs = append(errs, fmt.Errorf("unknown codec %q", p.Codec))
	}
	if p.Encoder != "auto" {
		if c, ok := EncoderCodec[p.Encoder]; !ok {
			errs = append(errs, fmt.Errorf("unknown encoder %q", p.Encoder))
		} else if c != p.Codec {
			errs = append(errs, fmt.Errorf("the encoder %s cannot encode %s", p.Encoder, p.Codec))
		}
	}
	maxQ := 51
	if p.Codec == "av1" {
		maxQ = 63
	}
	if p.Quality < 1 || p.Quality > maxQ {
		errs = append(errs, fmt.Errorf("the quality must be from 1 to %d", maxQ))
	}
	if p.MaxBitrateK < 0 || p.MaxBitrateK > 200_000 {
		errs = append(errs, errors.New("the maximum bitrate must be from 0 to 200000 kbit/s"))
	}
	if p.Preset != "" && !slices.Contains(x264Presets, p.Preset) {
		if n, err := strconv.Atoi(p.Preset); err != nil || n < 0 || n > 13 {
			errs = append(errs, fmt.Errorf("unknown preset %q", p.Preset))
		}
	}
	if p.MaxHeight != 0 && (p.MaxHeight < 144 || p.MaxHeight > 4320) {
		errs = append(errs, errors.New("the maximum height must be 0 or from 144 to 4320"))
	}
	if p.MaxFPS < 0 || p.MaxFPS > 240 {
		errs = append(errs, errors.New("the maximum frame rate must be from 0 to 240"))
	}
	if p.TenBit && p.Codec == "h264" {
		errs = append(errs, errors.New("browsers do not play 10-bit H.264"))
	}
	if p.AudioBitrateK < 32 || p.AudioBitrateK > 512 {
		errs = append(errs, errors.New("the audio bitrate must be from 32 to 512 kbit/s"))
	}
	if l := len(p.AudioLang); l != 0 && (l < 2 || l > 3) {
		errs = append(errs, errors.New("the audio language must be an ISO 639 code, for example eng"))
	}
	return errors.Join(errs...)
}

// Preset is a built-in profile.
type Preset struct {
	Name     string
	Policy   string
	Priority int
	Params   Params
}

// Presets are the built-in profiles that YAMP creates on the first start.
var Presets = []Preset{
	{
		// The default. The bitrate cap keeps streams practical over the internet.
		//
		// With copy-if-compatible this profile also covers remux and audio-only
		// jobs: it copies what it can and encodes the rest.
		Name: "High quality HEVC (Apple)", Policy: PolicyWhenNeeded, Priority: 20,
		Params: Params{
			VideoMode: VideoCopyIfCompatible, Codec: "hevc", Encoder: "auto", Quality: 20,
			MaxBitrateK: 25000, Preset: "slow", MaxHeight: 2160, TenBit: true,
			AudioBitrateK: 384,
		},
	},
	{
		// The fallback for browsers without HEVC.
		Name: "Compatible 1080p H.264", Policy: PolicyManual, Priority: 10,
		Params: Params{
			VideoMode: VideoCopyIfCompatible, Codec: "h264", Encoder: "auto", Quality: 21,
			MaxBitrateK: 8000, Preset: "medium", MaxHeight: 1080, Tonemap: true,
			AudioBitrateK: 160, Stereo: true,
		},
	},
	{
		Name: "Mobile 720p", Policy: PolicyManual, Priority: 5,
		Params: Params{
			VideoMode: VideoCopyIfCompatible, Codec: "h264", Encoder: "auto", Quality: 23,
			MaxBitrateK: 3000, Preset: "medium", MaxHeight: 720, Tonemap: true,
			AudioBitrateK: 128, Stereo: true,
		},
	},
	{
		Name: "Efficient HEVC", Policy: PolicyManual, Priority: 1,
		Params: Params{
			VideoMode: VideoEncode, Codec: "hevc", Encoder: "auto", Quality: 24,
			Preset: "medium", MaxHeight: 2160, TenBit: true, Tonemap: false,
			AudioBitrateK: 160, Stereo: true,
		},
	},
}
