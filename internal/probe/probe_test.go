package probe

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// dump formats b the way ffprobe -show_data does.
func dump(b []byte) string {
	var sb strings.Builder
	sb.WriteString("\n")
	for off := 0; off < len(b); off += 16 {
		end := min(off+16, len(b))
		var hexPart strings.Builder
		for i := off; i < off+16; i += 2 {
			if i > off {
				hexPart.WriteByte(' ')
			}
			for j := i; j < i+2; j++ {
				if j < end {
					fmt.Fprintf(&hexPart, "%02x", b[j])
				} else {
					hexPart.WriteString("  ")
				}
			}
		}
		fmt.Fprintf(&sb, "%08x: %s  %s\n", off, hexPart.String(), strings.Repeat(".", end-off))
	}
	return sb.String()
}

func TestExtradataRealDump(t *testing.T) {
	// Copied from ffprobe 7.1 output for a libx264 stream.
	s := Stream{ExtradataDump: "\n00000000: 01f4 000d ffe1 0019 67f4 000d 919b 2828  ........g.....((\n" +
		"00000010: 3f60 2200 0003 0002 0000 0300 641e 2853  ?`\".........d.(S\n" +
		"00000020: 2c01 0006 68eb e3c4 4844 fff8 f800       ,...h...HD....\n"}
	x := s.Extradata()
	if len(x) != 46 {
		t.Fatalf("len = %d, want 46", len(x))
	}
	if !bytes.Equal(x[:4], []byte{0x01, 0xf4, 0x00, 0x0d}) {
		t.Fatalf("prefix = % x", x[:4])
	}
	if got := avcString(&s); got != "avc1.F4000D" {
		t.Fatalf("avc = %q", got)
	}
}

func TestCodecStrings(t *testing.T) {
	hvcc := []byte{0x01, 0x02, 0x20, 0x00, 0x00, 0x00, 0x90, 0, 0, 0, 0, 0, 0x3f, 0xf0}
	tests := []struct {
		name string
		s    Stream
		webm bool
		want string
	}{
		{"avcC", Stream{CodecName: "h264", ExtradataDump: dump([]byte{1, 0x64, 0x00, 0x28, 0xff})}, false, "avc1.640028"},
		{"avc fallback", Stream{CodecName: "h264", Profile: "Main", Level: 31}, false, "avc1.4D401F"},
		{"hvcC", Stream{CodecName: "hevc", CodecTagString: "hvc1", ExtradataDump: dump(hvcc)}, false, "hvc1.2.4.L63.90"},
		{"hvcC hev1", Stream{CodecName: "hevc", CodecTagString: "hev1", ExtradataDump: dump(hvcc)}, false, "hev1.2.4.L63.90"},
		{"hevc main fallback", Stream{CodecName: "hevc", Profile: "Main", Level: 93}, false, "hvc1.1.6.L93.90"},
		{"av1C", Stream{CodecName: "av1", ExtradataDump: dump([]byte{0x81, 0x01, 0x0c, 0x00})}, false, "av01.0.01M.08"},
		{"av1C 10-bit high tier", Stream{CodecName: "av1", ExtradataDump: dump([]byte{0x81, 0x08, 0xC0, 0x00})}, false, "av01.0.08H.10"},
		{"vp9 mp4", Stream{CodecName: "vp9", Profile: "Profile 0", Width: 1920, Height: 1080, AvgFrameRate: "24/1", PixFmt: "yuv420p"}, false, "vp09.00.40.08"},
		{"vp9 webm", Stream{CodecName: "vp9"}, true, "vp9"},
		{"aac lc", Stream{CodecName: "aac", Profile: "LC"}, false, "mp4a.40.2"},
		{"he-aac", Stream{CodecName: "aac", Profile: "HE-AAC"}, false, "mp4a.40.5"},
		{"aac main", Stream{CodecName: "aac", Profile: "Main"}, false, "mp4a.40.1"},
		{"xhe-aac", Stream{CodecName: "aac", Profile: "xHE-AAC"}, false, "mp4a.40.42"},
		{"aac unknown profile", Stream{CodecName: "aac"}, false, "mp4a.40.2"},
		{"opus", Stream{CodecName: "opus"}, true, "opus"},
		{"flac mp4", Stream{CodecName: "flac"}, false, "fLaC"},
		{"unknown", Stream{CodecName: "mpeg2video"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CodecString(&tt.s, tt.webm); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTypeAttr(t *testing.T) {
	if got := TypeAttr("video/mp4", "avc1.640028, mp4a.40.2"); got != `video/mp4; codecs="avc1.640028, mp4a.40.2"` {
		t.Fatal(got)
	}
	if got := TypeAttr("video/mp4", ""); got != "video/mp4" {
		t.Fatal(got)
	}
}

func box(typ string, payload int) []byte {
	b := make([]byte, 8+payload)
	binary.BigEndian.PutUint32(b, uint32(len(b))) //nolint:gosec // G115: small test boxes
	copy(b[4:], typ)
	return b
}

func TestMoovFirst(t *testing.T) {
	fast := append(append(box("ftyp", 16), box("moov", 100)...), box("mdat", 1000)...)
	slow := append(append(box("ftyp", 16), box("mdat", 1000)...), box("moov", 100)...)
	// A 64-bit mdat box before moov.
	large := box("ftyp", 16)
	l := make([]byte, 16+32)
	binary.BigEndian.PutUint32(l, 1)
	copy(l[4:], "free")
	binary.BigEndian.PutUint64(l[8:], uint64(len(l)))
	large = append(append(large, l...), box("moov", 10)...)

	for name, tc := range map[string]struct {
		data []byte
		want bool
	}{
		"fast":       {fast, true},
		"slow":       {slow, false},
		"largesize":  {large, true},
		"not mp4":    {[]byte("\x1aE\xdf\xa3 matroska data......"), false},
		"truncated":  {fast[:4], false},
		"zero sized": {make([]byte, 64), false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := MoovFirst(bytes.NewReader(tc.data), int64(len(tc.data)))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	h264 := Stream{CodecType: "video", CodecName: "h264", PixFmt: "yuv420p", FieldOrder: "progressive",
		Profile: "High", Level: 40, Disposition: map[string]int{"default": 1}}
	h264hi10 := h264
	h264hi10.PixFmt, h264hi10.Profile = "yuv420p10le", "High 10"
	interlaced := h264
	interlaced.FieldOrder = "tt"
	hdr := h264
	hdr.ColorTransfer = "smpte2084"
	hvc1 := Stream{CodecType: "video", CodecName: "hevc", CodecTagString: "hvc1", PixFmt: "yuv420p10le", Profile: "Main 10", Level: 120}
	hev1 := hvc1
	hev1.CodecTagString = "hev1"
	hevc422 := hvc1
	hevc422.PixFmt = "yuv422p10le"
	vp9 := Stream{CodecType: "video", CodecName: "vp9", PixFmt: "yuv420p", Profile: "Profile 0"}
	mpeg2 := Stream{CodecType: "video", CodecName: "mpeg2video", PixFmt: "yuv420p"}
	cover := Stream{CodecType: "video", CodecName: "mjpeg", Disposition: map[string]int{"attached_pic": 1}}
	aac := Stream{CodecType: "audio", CodecName: "aac", Profile: "LC"}
	aacMain := aac
	aacMain.Profile = "Main"
	ac3 := Stream{CodecType: "audio", CodecName: "ac3"}
	opus := Stream{CodecType: "audio", CodecName: "opus"}

	mp4 := Format{FormatName: "mov,mp4,m4a,3gp,3g2,mj2", Tags: map[string]string{"major_brand": "isom"}}
	mkv := Format{FormatName: "matroska,webm"}

	tests := []struct {
		name      string
		path      string
		format    Format
		streams   []Stream
		moovFirst bool
		want      Class
		mime      string
	}{
		{"direct", "a.mp4", mp4, []Stream{h264, aac}, true, Direct, "video/mp4"},
		{"direct no audio", "a.mp4", mp4, []Stream{cover, h264}, true, Direct, "video/mp4"},
		{"moov at end", "a.mp4", mp4, []Stream{h264, aac}, false, Remux, ""},
		{"mkv h264 aac", "a.mkv", mkv, []Stream{h264, aac}, false, Remux, ""},
		{"mov", "a.mov", mp4, []Stream{h264, aac}, true, Remux, ""},
		{"mkv h264 ac3", "a.mkv", mkv, []Stream{h264, ac3}, false, AudioOnly, ""},
		{"mp4 h264 ac3", "a.mp4", mp4, []Stream{h264, ac3}, true, AudioOnly, ""},
		{"mp4 h264 aac main", "a.mp4", mp4, []Stream{h264, aacMain}, true, AudioOnly, ""},
		{"hi10p", "a.mkv", mkv, []Stream{h264hi10, aac}, false, Transcode, ""},
		{"interlaced", "a.mp4", mp4, []Stream{interlaced, aac}, true, Transcode, ""},
		{"hdr", "a.mp4", mp4, []Stream{hdr, aac}, true, Transcode, ""},
		{"hevc hvc1", "a.mp4", mp4, []Stream{hvc1, aac}, true, Conditional, "video/mp4"},
		{"hevc 4:2:2", "a.mp4", mp4, []Stream{hevc422, aac}, true, Transcode, ""},
		{"hevc hev1", "a.mp4", mp4, []Stream{hev1, aac}, true, Transcode, ""},
		{"hevc mkv", "a.mkv", mkv, []Stream{hvc1, aac}, false, Transcode, ""},
		{"vp9 webm", "a.webm", mkv, []Stream{vp9, opus}, false, Conditional, "video/webm"},
		{"mpeg2", "a.ts", Format{FormatName: "mpegts"}, []Stream{mpeg2, ac3}, false, Transcode, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Result{Streams: tt.streams, Format: tt.format}
			a, err := Classify(r, tt.path, tt.moovFirst)
			if err != nil {
				t.Fatal(err)
			}
			if a.Class != tt.want || a.Mime != tt.mime {
				t.Fatalf("got class %q mime %q, want %q %q", a.Class, a.Mime, tt.want, tt.mime)
			}
			if a.Video == nil || a.Video.CodecName == "mjpeg" {
				t.Fatal("cover art chosen as video")
			}
		})
	}

	if _, err := Classify(&Result{Streams: []Stream{aac}, Format: mp4}, "a.mp4", true); err != ErrNoVideo {
		t.Fatalf("audio-only file: %v", err)
	}
}

func TestAudioSelection(t *testing.T) {
	r := &Result{Streams: []Stream{
		{Index: 0, CodecType: "video"},
		{Index: 1, CodecType: "audio", Tags: map[string]string{"language": "pol"}},
		{Index: 2, CodecType: "audio", Tags: map[string]string{"language": "eng"}, Disposition: map[string]int{"default": 1}},
		{Index: 3, CodecType: "audio", Tags: map[string]string{"language": "jpn"}},
	}}
	if got := r.Audio("jpn").Index; got != 3 {
		t.Errorf("preferred language: got stream %d", got)
	}
	if got := r.Audio("").Index; got != 2 {
		t.Errorf("default: got stream %d", got)
	}
	if got := r.Audio("deu").Index; got != 2 {
		t.Errorf("missing language falls back to default: got stream %d", got)
	}
}

func TestDuration(t *testing.T) {
	r := &Result{Format: Format{Duration: "12.5"}}
	if got := r.Duration().Milliseconds(); got != 12500 {
		t.Fatalf("duration = %d ms", got)
	}
}

func TestDisplaySize(t *testing.T) {
	for rotation, want := range map[string][2]int{
		"0": {1920, 1080}, "90": {1080, 1920}, "-90": {1080, 1920}, "180": {1920, 1080}, "270": {1080, 1920},
	} {
		r, err := Parse([]byte(`{"streams":[{"codec_type":"video","width":1920,"height":1080,` +
			`"side_data_list":[{"side_data_type":"Display Matrix","rotation":` + rotation + `}]}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if w, h := r.Video().DisplaySize(); w != want[0] || h != want[1] {
			t.Errorf("rotation %s: %dx%d, want %dx%d", rotation, w, h, want[0], want[1])
		}
	}
}
