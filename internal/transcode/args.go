package transcode

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/amwolff/yamp/internal/probe"
)

// TonemapChain converts PQ or HLG to SDR BT.709 in software. The input tags are
// explicit because many files lack some of them.
func TonemapChain(transfer string) string {
	return "zscale=tin=" + transfer + ":pin=bt2020:min=bt2020nc:t=linear:npl=100,format=gbrpf32le," +
		"zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p"
}

// keyframeInterval is the keyframe distance in seconds. A fixed GOP lets a
// later HLS feature repackage renditions without a new encode.
const keyframeInterval = 2

// Input is everything BuildArgs needs to know about one job.
type Input struct {
	Path   string
	Output string
	Probe  *probe.Result
	Params Params
	// Encoder is the resolved encoder; it is ignored when the video is copied.
	Encoder string
	Caps    *Caps
}

// Plan is the result of BuildArgs.
type Plan struct {
	Args      []string
	CopyVideo bool
	CopyAudio bool
	// Encoder is "copy" or the video encoder.
	Encoder string
	Width   int
	Height  int
}

// ErrNoEncoder means the video needs an encode but no encoder is available.
var ErrNoEncoder = errors.New("no encoder available")

// NeedsVideoEncode reports whether the profile encodes the video, i.e. whether
// an encoder must be resolved.
func NeedsVideoEncode(r *probe.Result, p Params) bool {
	v := r.Video()
	return v == nil || !canCopyVideo(r, v, p)
}

// BuildArgs returns the ffmpeg arguments for one job: one video and one audio
// stream, no metadata, a keyframe every 2 s, faststart MP4.
func BuildArgs(in Input) (Plan, error) {
	p := in.Params
	v := in.Probe.Video()
	if v == nil {
		return Plan{}, probe.ErrNoVideo
	}
	a := in.Probe.Audio(p.AudioLang)
	plan := Plan{CopyVideo: canCopyVideo(in.Probe, v, p)}
	plan.CopyAudio = a != nil && a.CodecName == "aac" && (!p.Stereo || a.Channels <= 2)
	if !plan.CopyVideo && in.Encoder == "" {
		return Plan{}, ErrNoEncoder
	}
	plan.Encoder = "copy"
	if !plan.CopyVideo {
		plan.Encoder = in.Encoder
	}

	fps := v.FPS()
	if fps <= 0 || fps > 240 {
		fps = 25
	}
	outFPS := fps
	if p.MaxFPS > 0 && fps > float64(p.MaxFPS)+0.01 {
		outFPS = float64(p.MaxFPS)
	}
	plan.Width, plan.Height = v.Width, v.Height
	if !plan.CopyVideo {
		w, h := v.DisplaySize()
		plan.Width, plan.Height = outputSize(w, h, p.MaxHeight)
	}

	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-nostats", "-stats_period", "1", "-progress", "pipe:1", "-y",
	}
	if !plan.CopyVideo && isVAAPI(in.Encoder) {
		args = append(args, "-init_hw_device", "vaapi=va:"+in.Caps.VAAPIDevice, "-filter_hw_device", "va")
	}
	args = append(args, "-i", in.Path, "-map", fmt.Sprintf("0:%d", v.Index))
	if a != nil {
		args = append(args, "-map", fmt.Sprintf("0:%d", a.Index))
	}
	args = append(args, "-map_metadata", "-1", "-map_chapters", "-1", "-sn", "-dn")

	if plan.CopyVideo {
		args = append(args, "-c:v", "copy")
		if v.CodecName == "hevc" {
			args = append(args, "-tag:v", "hvc1")
		}
	} else {
		filters := videoFilters(in, v, fps, outFPS, plan.Width, plan.Height)
		args = append(args, "-vf", strings.Join(filters, ","))
		args = append(args, encoderArgs(in.Encoder, p, outFPS, plan.Width, plan.Height)...)
	}

	switch {
	case a == nil:
		args = append(args, "-an")
	case plan.CopyAudio:
		args = append(args, "-c:a", "copy")
	default:
		args = append(args, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", p.AudioBitrateK))
		if p.Stereo {
			args = append(args, "-ac", "2")
		} else if a.Channels > 6 {
			args = append(args, "-ac", "6")
		}
	}
	args = append(args, "-movflags", "+faststart", "-f", "mp4", in.Output)
	plan.Args = args
	return plan, nil
}

// canCopyVideo reports whether the source video already meets the profile.
func canCopyVideo(r *probe.Result, v *probe.Stream, p Params) bool {
	if p.VideoMode != VideoCopyIfCompatible {
		return false
	}
	switch p.Codec {
	case "h264":
		if !probe.H264Compatible(v) {
			return false
		}
	case "hevc":
		// Every HEVC decoder also decodes H.264, so compatible H.264 stays.
		if probe.H264Compatible(v) {
			break
		}
		if v.CodecName != "hevc" || v.Interlaced() || v.BitDepth() > 10 || (v.HDR() && p.Tonemap) {
			return false
		}
	case "av1":
		if v.CodecName != "av1" || v.Interlaced() || (v.HDR() && p.Tonemap) {
			return false
		}
	default:
		return false
	}
	// Like outputSize, the limit applies to the shorter side.
	if p.MaxHeight > 0 && min(v.Width, v.Height) > p.MaxHeight {
		return false
	}
	if p.MaxFPS > 0 && v.FPS() > float64(p.MaxFPS)+0.01 {
		return false
	}
	if p.MaxBitrateK > 0 {
		rate := v.StreamBitRate()
		if rate == 0 {
			rate = r.BitRate()
		}
		// Allow some headroom: the cap is for new encodes, and a source a
		// little above it is not worth a full encode.
		if rate > int64(p.MaxBitrateK)*1000*5/4 {
			return false
		}
	}
	return true
}

// outputSize fits (w, h) into a 16:9 box (9:16 for portrait) whose short side
// is maxHeight, keeping the aspect ratio and even dimensions.
func outputSize(w, h, maxHeight int) (int, int) {
	if w <= 0 || h <= 0 {
		return w, h
	}
	if maxHeight > 0 {
		boxW, boxH := maxHeight*16/9, maxHeight
		if h > w {
			boxW, boxH = boxH, boxW
		}
		scale := math.Min(1, math.Min(float64(boxW)/float64(w), float64(boxH)/float64(h)))
		w = int(math.Round(float64(w) * scale))
		h = int(math.Round(float64(h) * scale))
	}
	return w &^ 1, h &^ 1
}

func videoFilters(in Input, v *probe.Stream, fps, outFPS float64, w, h int) []string {
	var f []string
	if v.Interlaced() {
		f = append(f, "bwdif=mode=send_frame")
	}
	if outFPS < fps {
		f = append(f, "fps="+strconv.FormatFloat(outFPS, 'f', -1, 64))
	}
	if v.HDR() && in.Params.Tonemap && in.Caps.CanTonemap() {
		f = append(f, TonemapChain(v.ColorTransfer))
	}
	if dw, dh := v.DisplaySize(); w != dw || h != dh {
		f = append(f, fmt.Sprintf("scale=%d:%d", w, h))
	}
	tenBit := in.Params.TenBit && in.Params.Codec != "h264"
	switch {
	case isVAAPI(in.Encoder):
		pix := "nv12"
		if tenBit {
			pix = "p010"
		}
		f = append(f, "format="+pix, "hwupload")
	case isQSV(in.Encoder):
		pix := "nv12"
		if tenBit {
			pix = "p010le"
		}
		f = append(f, "format="+pix)
	default:
		pix := "yuv420p"
		if tenBit {
			pix = "yuv420p10le"
		}
		f = append(f, "format="+pix)
	}
	return f
}

func encoderArgs(enc string, p Params, fps float64, w, h int) []string {
	gop := strconv.Itoa(int(math.Round(fps * keyframeInterval)))
	forceKeys := []string{"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", keyframeInterval)}
	quality := strconv.Itoa(p.Quality)
	var rate []string
	if p.MaxBitrateK > 0 {
		rate = []string{"-maxrate", fmt.Sprintf("%dk", p.MaxBitrateK), "-bufsize", fmt.Sprintf("%dk", 2*p.MaxBitrateK)}
	}
	preset := p.Preset
	var args []string
	switch enc {
	case "libx264":
		if preset == "" {
			preset = "medium"
		}
		args = []string{"-c:v", "libx264", "-preset", preset, "-crf", quality,
			"-profile:v", "high", "-level:v", h264Level(w, h, fps)}
		args = append(args, rate...)
		args = append(args, forceKeys...)
		args = append(args, "-sc_threshold", "0")
	case "libx265":
		if preset == "" {
			preset = "medium"
		}
		args = []string{"-c:v", "libx265", "-preset", preset, "-crf", quality, "-tag:v", "hvc1"}
		args = append(args, rate...)
		args = append(args, forceKeys...)
		args = append(args, "-x265-params", "scenecut=0:open-gop=0:log-level=error")
	case "libsvtav1":
		if _, err := strconv.Atoi(preset); err != nil {
			preset = "6"
		}
		args = []string{"-c:v", "libsvtav1", "-preset", preset, "-crf", quality, "-g", gop}
		if p.MaxBitrateK > 0 {
			args = append(args, "-svtav1-params", fmt.Sprintf("mbr=%d", p.MaxBitrateK))
		}
	case "h264_qsv", "hevc_qsv":
		if !qsvPreset(preset) {
			preset = "medium"
		}
		args = []string{"-c:v", enc, "-preset", preset, "-global_quality", quality, "-g", gop, "-forced_idr", "1"}
		if enc == "h264_qsv" {
			args = append(args, "-profile:v", "high")
		} else {
			args = append(args, "-tag:v", "hvc1")
			if p.TenBit {
				args = append(args, "-profile:v", "main10")
			}
		}
		args = append(args, rate...)
	case "h264_vaapi", "hevc_vaapi":
		args = []string{"-c:v", enc, "-g", gop, "-idr_interval", "1"}
		if p.MaxBitrateK > 0 {
			args = append(args, "-rc_mode", "VBR", "-b:v", fmt.Sprintf("%dk", p.MaxBitrateK*3/4),
				"-maxrate", fmt.Sprintf("%dk", p.MaxBitrateK))
		} else {
			args = append(args, "-rc_mode", "CQP", "-qp", quality)
		}
		if enc == "h264_vaapi" {
			args = append(args, "-profile:v", "high")
		} else {
			args = append(args, "-tag:v", "hvc1")
			if p.TenBit {
				args = append(args, "-profile:v", "main10")
			}
		}
	}
	return args
}

func qsvPreset(p string) bool {
	switch p {
	case "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow":
		return true
	}
	return false
}

// h264Level picks the lowest H.264 level that fits the frame size and rate.
func h264Level(w, h int, fps float64) string {
	mbs := float64(((w + 15) / 16) * ((h + 15) / 16))
	levels := []struct {
		name          string
		maxFS, maxMBs float64
	}{
		{"3.0", 1620, 40500},
		{"3.1", 3600, 108000},
		{"3.2", 5120, 216000},
		{"4.0", 8192, 245760},
		{"4.1", 8192, 245760},
		{"4.2", 8704, 522240},
		{"5.0", 22080, 589824},
		{"5.1", 36864, 983040},
		{"5.2", 36864, 2073600},
	}
	for _, l := range levels {
		// Level 4.0 and 4.1 have the same frame limits; 4.1 allows the higher
		// bitrates that CRF encodes can reach.
		if l.name == "4.0" {
			continue
		}
		if mbs <= l.maxFS && mbs*fps <= l.maxMBs {
			return l.name
		}
	}
	return "6.0"
}
