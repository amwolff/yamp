package transcode

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amwolff/yamp/internal/probe"
)

func TestPresetsValid(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Presets {
		if err := p.Params.Validate(); err != nil {
			t.Errorf("%s: %v", p.Name, err)
		}
		if seen[p.Params.Hash()] {
			t.Errorf("%s: duplicate hash", p.Name)
		}
		seen[p.Params.Hash()] = true
	}
}

func TestParamsHash(t *testing.T) {
	a := Presets[0].Params
	b := a
	if a.Hash() != b.Hash() {
		t.Fatal("equal params, different hash")
	}
	b.Quality++
	if a.Hash() == b.Hash() {
		t.Fatal("changed params, same hash")
	}
	parsed, err := ParseParams(a.JSON())
	if err != nil || parsed != a {
		t.Fatalf("JSON round trip: %+v, %v", parsed, err)
	}
}

func TestParamsValidate(t *testing.T) {
	bad := []func(*Params){
		func(p *Params) { p.Codec = "mpeg2" },
		func(p *Params) { p.Encoder = "libx265" }, // wrong codec for h264
		func(p *Params) { p.Quality = 0 },
		func(p *Params) { p.Preset = "warp" },
		func(p *Params) { p.MaxHeight = 100 },
		func(p *Params) { p.TenBit = true },
		func(p *Params) { p.AudioBitrateK = 8 },
		func(p *Params) { p.AudioLang = "english" },
		func(p *Params) { p.VideoMode = "maybe" },
	}
	for i, mutate := range bad {
		p := Presets[0].Params
		mutate(&p)
		if p.Validate() == nil {
			t.Errorf("case %d: invalid params accepted: %+v", i, p)
		}
	}
}

func caps(available ...string) *Caps {
	c := &Caps{Filters: map[string]bool{"zscale": true, "tonemap": true}, VAAPIDevice: "/dev/dri/renderD128"}
	for _, e := range candidates {
		c.Encoders = append(c.Encoders, EncoderStatus{Name: e, Available: slices.Contains(available, e)})
	}
	return c
}

func stream(codec string, mods ...func(*probe.Stream)) probe.Stream {
	s := probe.Stream{CodecName: codec, Disposition: map[string]int{"default": 1}}
	switch codec {
	case "h264", "hevc", "av1", "vp9", "mpeg2video":
		s.CodecType = "video"
		s.PixFmt = "yuv420p"
		s.Width, s.Height = 1920, 1080
		s.AvgFrameRate = "24000/1001"
		s.FieldOrder = "progressive"
	default:
		s.CodecType = "audio"
		s.Channels = 2
	}
	for _, m := range mods {
		m(&s)
	}
	return s
}

func result(streams ...probe.Stream) *probe.Result {
	for i := range streams {
		streams[i].Index = i
	}
	return &probe.Result{Streams: streams, Format: probe.Format{Duration: "60", BitRate: "5000000"}}
}

func argString(t *testing.T, in Input) (Plan, string) {
	t.Helper()
	if in.Caps == nil {
		in.Caps = caps("libx264", "libx265", "libsvtav1")
	}
	in.Path, in.Output = "IN", "OUT"
	plan, err := BuildArgs(in)
	if err != nil {
		t.Fatal(err)
	}
	return plan, strings.Join(plan.Args, " ")
}

const prefix = "-nostdin -hide_banner -loglevel error -nostats -stats_period 1 -progress pipe:1 -y "

func TestBuildArgsGolden(t *testing.T) {
	compat := Presets[0].Params
	hevc := Presets[2].Params
	tests := []struct {
		name    string
		in      Input
		want    string
		encoder string
	}{
		{
			name: "remux h264 aac",
			in:   Input{Probe: result(stream("h264"), stream("aac")), Params: compat},
			want: prefix + "-i IN -map 0:0 -map 0:1 -map_metadata -1 -map_chapters -1 -sn -dn " +
				"-c:v copy -c:a copy -movflags +faststart -f mp4 OUT",
			encoder: "copy",
		},
		{
			name: "audio only ac3 5.1",
			in:   Input{Probe: result(stream("h264"), stream("ac3", func(s *probe.Stream) { s.Channels = 6 })), Params: compat},
			want: prefix + "-i IN -map 0:0 -map 0:1 -map_metadata -1 -map_chapters -1 -sn -dn " +
				"-c:v copy -c:a aac -b:a 160k -ac 2 -movflags +faststart -f mp4 OUT",
			encoder: "copy",
		},
		{
			name: "4k hdr hevc to 1080p h264 x264",
			in: Input{Probe: result(stream("hevc", func(s *probe.Stream) {
				s.Width, s.Height, s.PixFmt, s.ColorTransfer = 3840, 2160, "yuv420p10le", "smpte2084"
			}), stream("eac3")), Params: compat, Encoder: "libx264"},
			want: prefix + "-i IN -map 0:0 -map 0:1 -map_metadata -1 -map_chapters -1 -sn -dn " +
				"-vf " + tonemapChain("smpte2084") + ",scale=1920:1080,format=yuv420p " +
				"-c:v libx264 -preset medium -crf 21 -profile:v high -level:v 4.1 -maxrate 8000k -bufsize 16000k " +
				"-force_key_frames expr:gte(t,n_forced*2) -sc_threshold 0 " +
				"-c:a aac -b:a 160k -ac 2 -movflags +faststart -f mp4 OUT",
			encoder: "libx264",
		},
		{
			name: "interlaced h264 with qsv",
			in: Input{Probe: result(stream("h264", func(s *probe.Stream) { s.FieldOrder = "tt"; s.AvgFrameRate = "25/1" }), stream("aac")),
				Params: compat, Encoder: "h264_qsv", Caps: caps("h264_qsv", "libx264")},
			want: prefix + "-i IN -map 0:0 -map 0:1 -map_metadata -1 -map_chapters -1 -sn -dn " +
				"-vf bwdif=mode=send_frame,format=nv12 " +
				"-c:v h264_qsv -preset medium -global_quality 21 -g 50 -forced_idr 1 -profile:v high -maxrate 8000k -bufsize 16000k " +
				"-c:a copy -movflags +faststart -f mp4 OUT",
			encoder: "h264_qsv",
		},
		{
			name: "vaapi hevc 10-bit",
			in: Input{Probe: result(stream("h264"), stream("aac")), Params: hevc, Encoder: "hevc_vaapi",
				Caps: caps("hevc_vaapi")},
			want: prefix + "-init_hw_device vaapi=va:/dev/dri/renderD128 -filter_hw_device va " +
				"-i IN -map 0:0 -map 0:1 -map_metadata -1 -map_chapters -1 -sn -dn " +
				"-vf format=p010,hwupload -c:v hevc_vaapi -g 48 -idr_interval 1 -rc_mode CQP -qp 24 -tag:v hvc1 -profile:v main10 " +
				"-c:a copy -movflags +faststart -f mp4 OUT",
			encoder: "hevc_vaapi",
		},
		{
			name: "x265 no audio",
			in:   Input{Probe: result(stream("h264")), Params: hevc, Encoder: "libx265"},
			want: prefix + "-i IN -map 0:0 -map_metadata -1 -map_chapters -1 -sn -dn " +
				"-vf format=yuv420p10le -c:v libx265 -preset medium -crf 24 -tag:v hvc1 " +
				"-force_key_frames expr:gte(t,n_forced*2) -x265-params scenecut=0:open-gop=0:log-level=error " +
				"-an -movflags +faststart -f mp4 OUT",
			encoder: "libx265",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, got := argString(t, tt.in)
			if got != tt.want {
				t.Fatalf("args mismatch\n got: %s\nwant: %s", got, tt.want)
			}
			if plan.Encoder != tt.encoder {
				t.Fatalf("encoder = %q, want %q", plan.Encoder, tt.encoder)
			}
		})
	}
}

func TestBuildArgsInvariants(t *testing.T) {
	// Every plan strips metadata and writes a faststart MP4; HEVC always gets
	// the hvc1 tag.
	in := Input{Probe: result(stream("hevc", func(s *probe.Stream) { s.CodecTagString = "hev1" }), stream("aac")),
		Params: Params{VideoMode: VideoCopyIfCompatible, Codec: "hevc", Encoder: "auto", Quality: 24, AudioBitrateK: 128}}
	plan, got := argString(t, in)
	if !plan.CopyVideo {
		t.Fatal("expected video copy")
	}
	for _, want := range []string{"-map_metadata -1", "-tag:v hvc1", "-movflags +faststart"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestCopyDecisions(t *testing.T) {
	compat := Presets[0].Params
	mobile := Presets[1].Params
	tests := []struct {
		name string
		v    probe.Stream
		p    Params
		copy bool
	}{
		{"compatible 1080p", stream("h264"), compat, true},
		{"too tall for mobile", stream("h264"), mobile, false},
		{"hi10p", stream("h264", func(s *probe.Stream) { s.PixFmt = "yuv420p10le" }), compat, false},
		{"bitrate above cap", stream("h264", func(s *probe.Stream) { s.BitRate = "20000000" }), compat, false},
		{"encode mode", stream("h264"), Params{VideoMode: VideoEncode, Codec: "h264"}, false},
		{"hevc for h264 profile", stream("hevc"), compat, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := result(tt.v)
			if got := canCopyVideo(r, r.Video(), tt.p); got != tt.copy {
				t.Fatalf("copy = %v, want %v", got, tt.copy)
			}
			if NeedsVideoEncode(r, tt.p) == tt.copy {
				t.Fatal("NeedsVideoEncode disagrees with canCopyVideo")
			}
		})
	}
}

func TestBuildArgsNoEncoder(t *testing.T) {
	_, err := BuildArgs(Input{Probe: result(stream("mpeg2video")), Params: Presets[0].Params, Caps: caps()})
	if !errors.Is(err, ErrNoEncoder) {
		t.Fatalf("err = %v", err)
	}
}

func TestOutputSize(t *testing.T) {
	tests := []struct{ w, h, max, ww, wh int }{
		{3840, 2160, 1080, 1920, 1080},
		{1920, 1080, 1080, 1920, 1080},
		{1280, 720, 1080, 1280, 720},
		{3840, 1606, 1080, 1920, 802},
		{1080, 1920, 1080, 1080, 1920}, // portrait fits a 9:16 box
		{2160, 3840, 1080, 1080, 1920},
		{1441, 1081, 720, 960, 720},
		{1921, 1081, 0, 1920, 1080}, // only made even
	}
	for _, tt := range tests {
		if w, h := outputSize(tt.w, tt.h, tt.max); w != tt.ww || h != tt.wh {
			t.Errorf("outputSize(%d, %d, %d) = %dx%d, want %dx%d", tt.w, tt.h, tt.max, w, h, tt.ww, tt.wh)
		}
	}
}

func TestH264Level(t *testing.T) {
	tests := []struct {
		w, h int
		fps  float64
		want string
	}{
		{1280, 720, 30, "3.1"},
		{1280, 720, 60, "3.2"},
		{1920, 1080, 24, "4.1"},
		{1920, 1080, 30, "4.1"},
		{1920, 1080, 60, "4.2"},
		{3840, 2160, 30, "5.1"},
		{3840, 2160, 60, "5.2"},
	}
	for _, tt := range tests {
		if got := h264Level(tt.w, tt.h, tt.fps); got != tt.want {
			t.Errorf("h264Level(%d, %d, %v) = %s, want %s", tt.w, tt.h, tt.fps, got, tt.want)
		}
	}
}

func TestParseProgress(t *testing.T) {
	in := "frame=10\nout_time_us=N/A\nspeed=N/A\nprogress=continue\n" +
		"out_time_us=1500000\nspeed=2.5x\nprogress=continue\n" +
		"out_time_ms=3000000\nprogress=end\n"
	var got []Progress
	if err := ParseProgress(strings.NewReader(in), func(p Progress) { got = append(got, p) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("blocks = %d", len(got))
	}
	if got[0].OutTime != 0 || got[1].OutTime != 1500*time.Millisecond || got[1].Speed != 2.5 {
		t.Fatalf("unexpected blocks: %+v", got)
	}
	if !got[2].End || got[2].OutTime != 3*time.Second {
		t.Fatalf("last block: %+v", got[2])
	}
}

func TestDetect(t *testing.T) {
	encoders := []byte(`Encoders:
 V..... = Video
 ------
 V....D libx264              libx264 H.264
 V....D h264_vaapi           H.264/AVC (VAAPI)
 V....D h264_qsv             H.264 (Intel Quick Sync Video acceleration)
 A....D aac                  AAC (Advanced Audio Coding)
`)
	filters := []byte(` ... zscale            V->V       Apply resizing.
 ... tonemap           V->V       Conversion to/from different dynamic ranges.
`)
	var calls [][]string
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, args)
		switch {
		case slices.Contains(args, "-encoders"):
			return encoders, nil
		case slices.Contains(args, "-filters"):
			return filters, nil
		case slices.Contains(args, "h264_qsv"):
			return nil, errors.New("Error creating a MFX session")
		}
		return nil, nil
	}
	c := Detect(context.Background(), run, "ffmpeg", "/dev/dri/renderD128")
	if !c.Has("libx264") || !c.Has("h264_vaapi") || c.Has("h264_qsv") || c.Has("libx265") {
		t.Fatalf("unexpected availability: %v", c.Available())
	}
	if !c.CanTonemap() {
		t.Fatal("filters not detected")
	}
	for _, args := range calls {
		if slices.Contains(args, "h264_vaapi") && !slices.Contains(args, "-init_hw_device") {
			t.Fatal("VAAPI test encode without a device")
		}
	}
	if enc, _ := c.Resolve("h264", "auto"); enc != "h264_vaapi" {
		t.Fatalf("auto resolved to %q", enc)
	}
	if _, err := c.Resolve("hevc", "auto"); err == nil {
		t.Fatal("hevc resolved without an encoder")
	}
	if _, err := c.Resolve("h264", "h264_qsv"); err == nil {
		t.Fatal("unavailable encoder resolved")
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 4}
	_, _ = tb.Write([]byte("abc"))
	_, _ = tb.Write([]byte("defg"))
	if got := string(tb.Bytes()); got != "defg" {
		t.Fatalf("tail = %q", got)
	}
}
