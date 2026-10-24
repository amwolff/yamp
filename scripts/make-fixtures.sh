#!/usr/bin/env bash
# Generates a small test library with one file for each playback class.
# Usage: scripts/make-fixtures.sh <dir> [seconds]
set -euo pipefail

dir=${1:?usage: make-fixtures.sh <dir> [seconds]}
dur=${2:-20}
mkdir -p "$dir/Movies" "$dir/Shows/Season 1" "$dir/Clips"

ff() { ffmpeg -nostdin -hide_banner -loglevel error -y "$@"; }
video() { echo -f lavfi -i "testsrc2=size=$1:rate=$2:duration=$dur"; }
audio() { echo -f lavfi -i "sine=frequency=$1:sample_rate=48000:duration=$dur"; }

srt=$(mktemp --suffix=.srt)
trap 'rm -f "$srt"' EXIT
printf '1\n00:00:01,000 --> 00:00:04,000\nHello from YAMP\n\n2\n00:00:05,000 --> 00:00:08,000\nSecond line\n' >"$srt"

# direct: H.264 + AAC in MP4 with faststart.
ff $(video 1280x720 30) $(audio 440) -c:v libx264 -preset veryfast -pix_fmt yuv420p \
  -c:a aac -b:a 128k -movflags +faststart "$dir/Movies/Big Buck Test (2024).mp4"
# remux: the same, moov at the end.
ff $(video 640x360 25) $(audio 550) -c:v libx264 -preset veryfast -pix_fmt yuv420p \
  -c:a aac -b:a 96k "$dir/Clips/slow_start_clip.mp4"
# remux: H.264 + AAC in Matroska, with a text subtitle track.
ff $(video 1280x720 24) $(audio 660) -i "$srt" -map 0 -map 1 -map 2 \
  -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac -b:a 128k -c:s srt \
  -metadata:s:a:0 language=eng -metadata:s:s:0 language=eng "$dir/Shows/Season 1/Show.S01E01.mkv"
# audio: H.264 + AC-3 5.1 in Matroska.
ff $(video 1280x720 24) $(audio 330) -c:v libx264 -preset veryfast -pix_fmt yuv420p \
  -c:a ac3 -ac 6 -b:a 384k "$dir/Shows/Season 1/Show.S01E02.mkv"
# transcode: 10-bit H.264.
ff $(video 1280x720 24) $(audio 770) -c:v libx264 -preset veryfast -pix_fmt yuv420p10le \
  -c:a aac "$dir/Movies/Ten Bit Movie.mkv"
# transcode: HEVC with the hev1 tag.
ff $(video 1280x720 24) $(audio 880) -c:v libx265 -preset ultrafast -pix_fmt yuv420p -tag:v hev1 \
  -x265-params log-level=error -c:a aac -movflags +faststart "$dir/Movies/hevc_hev1.mp4"
# conditional: VP9 + Opus in WebM.
ff $(video 854x480 30) $(audio 990) -c:v libvpx-vp9 -deadline realtime -cpu-used 8 -b:v 1M \
  -c:a libopus "$dir/Clips/web clip.webm"
# conditional + HDR: HEVC Main 10 with PQ tags and the hvc1 tag.
ff $(video 1920x1080 24) $(audio 220) -c:v libx265 -preset ultrafast -pix_fmt yuv420p10le -tag:v hvc1 \
  -x265-params log-level=error:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc \
  -color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc \
  -c:a aac -movflags +faststart "$dir/Movies/HDR Demo.mp4"
echo "fixtures written to $dir"
