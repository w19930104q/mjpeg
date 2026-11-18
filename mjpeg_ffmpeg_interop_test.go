package mjpeg

// Interoperability regression tests: AVI files produced by this package must
// be decodable by ffmpeg, frame by frame, exactly matching the JPEG frames
// that were written.
//
// The tests write fixed (deterministic) frame sequences, decode the resulting
// AVI with ffmpeg to raw RGB24, decode the source JPEG frames with ffmpeg the
// same way, and require the per-frame SHA-256 hashes to be identical.
//
// Covered: 4:2:0 / 4:2:2 / 4:4:4 chroma subsampling, multiple resolutions
// (including odd dimensions where the subsampling allows), multiple frame
// rates, and VFR (variable frame durations emulated by frame repetition,
// which is how VFR is expressed in a CFR MJPEG/AVI container).
//
// 4:2:0 input frames are encoded with Go's image/jpeg (the documented usage
// of this package). Go's JPEG encoder only supports 4:2:0, so the 4:2:2 and
// 4:4:4 input frames are encoded from the same deterministic source frames
// with ffmpeg's mjpeg encoder. The library under test only wraps JPEG bytes
// into an AVI container; what is verified is that ffmpeg decodes that
// container back to exactly the written frames.
//
// The tests are skipped if ffmpeg is not available.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// interopCase describes one fixed frame sequence to write and verify.
type interopCase struct {
	name   string
	ratio  image.YCbCrSubsampleRatio
	width  int
	height int
	fps    int32
	// repeats[i] is how many times source frame i is written to the AVI.
	// nil means every source frame is written exactly once (CFR).
	// A non-uniform pattern exercises VFR-by-duplication.
	repeats []int
}

var interopCases = []interopCase{
	// 4:2:0, several resolutions and frame rates
	{name: "420_320x240_30fps", ratio: image.YCbCrSubsampleRatio420, width: 320, height: 240, fps: 30},
	{name: "420_640x360_24fps", ratio: image.YCbCrSubsampleRatio420, width: 640, height: 360, fps: 24},
	{name: "420_160x120_60fps", ratio: image.YCbCrSubsampleRatio420, width: 160, height: 120, fps: 60},
	// 4:2:2, including a width not divisible by 2 (chroma is padded)
	{name: "422_320x240_30fps", ratio: image.YCbCrSubsampleRatio422, width: 320, height: 240, fps: 30},
	{name: "422_321x242_25fps", ratio: image.YCbCrSubsampleRatio422, width: 321, height: 242, fps: 25},
	// 4:4:4, including odd width and height
	{name: "444_320x240_30fps", ratio: image.YCbCrSubsampleRatio444, width: 320, height: 240, fps: 30},
	{name: "444_161x121_15fps", ratio: image.YCbCrSubsampleRatio444, width: 161, height: 121, fps: 15},
	// VFR: fixed source frames with variable display durations,
	// expressed as duplicated frames in the CFR container.
	{name: "420_vfr_320x240_30fps", ratio: image.YCbCrSubsampleRatio420, width: 320, height: 240, fps: 30,
		repeats: []int{1, 3, 1, 2, 1, 1, 4, 2, 1, 3}},
	{name: "444_vfr_200x112_30fps", ratio: image.YCbCrSubsampleRatio444, width: 200, height: 112, fps: 30,
		repeats: []int{2, 1, 5, 1, 1, 2, 3, 1, 2, 1}},
}

// numSrcFrames is the number of distinct source frames generated per case.
const numSrcFrames = 10

// makeSrcFrame builds the deterministic source frame with the given index.
// The content is a fixed function of (x, y, frame): a gradient plus a moving
// block, so frames are visually and hash-wise clearly distinct.
func makeSrcFrame(ratio image.YCbCrSubsampleRatio, width, height, frame int) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, width, height), ratio)

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Y[y*img.YStride+x] = uint8((x*2 + y*3 + frame*11) & 0xff)
		}
	}
	cw, ch := chromaDims(ratio, width, height)
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			img.Cb[y*img.CStride+x] = uint8((x*5 + y*7 + frame*13 + 64) & 0xff)
			img.Cr[y*img.CStride+x] = uint8((x*11 + y*3 + frame*7 + 192) & 0xff)
		}
	}

	// Moving block: 1/8 of the width/height, position depends on the frame.
	bw, bh := width/8+1, height/8+1
	bx := (frame * 7) % (width - bw + 1)
	by := (frame * 5) % (height - bh + 1)
	for y := by; y < by+bh; y++ {
		for x := bx; x < bx+bw; x++ {
			img.Y[y*img.YStride+x] = uint8((frame*37 + 200) & 0xff)
		}
	}

	return img
}

// chromaDims returns the chroma plane dimensions for the given subsampling.
func chromaDims(ratio image.YCbCrSubsampleRatio, w, h int) (cw, ch int) {
	switch ratio {
	case image.YCbCrSubsampleRatio420:
		return (w + 1) / 2, (h + 1) / 2
	case image.YCbCrSubsampleRatio422:
		return (w + 1) / 2, h
	case image.YCbCrSubsampleRatio440:
		return w, (h + 1) / 2
	default: // 444, 410, 411 (not used here)
		return w, h
	}
}

// encodeJPEG deterministically encodes a frame to JPEG with Go's image/jpeg.
func encodeJPEG(t *testing.T, img *image.YCbCr) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// yuvPixFmt maps a subsampling to ffmpeg's planar YUV pixel format name.
var yuvPixFmt = map[image.YCbCrSubsampleRatio]string{
	image.YCbCrSubsampleRatio420: "yuv420p",
	image.YCbCrSubsampleRatio422: "yuv422p",
	image.YCbCrSubsampleRatio444: "yuv444p",
}

// encodeJPEGsWithFFmpeg encodes frames to JPEG files (frame_0000.jpg, ...) in
// dir using ffmpeg's mjpeg encoder, and returns the JPEG data of each frame.
// Used for subsamplings Go's image/jpeg cannot encode (4:2:2, 4:4:4).
func encodeJPEGsWithFFmpeg(t *testing.T, ffmpeg, dir string, frames []*image.YCbCr) [][]byte {
	t.Helper()
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	w, h := frames[0].Rect.Dx(), frames[0].Rect.Dy()
	ratio := frames[0].SubsampleRatio
	pixFmt, ok := yuvPixFmt[ratio]
	if !ok {
		t.Fatalf("no ffmpeg pix fmt for subsample ratio %v", ratio)
	}

	// Write all frames as one planar YUV rawvideo stream.
	rawPath := filepath.Join(dir, "frames.yuv")
	f, err := os.Create(rawPath)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	cw, ch := chromaDims(ratio, w, h)
	for _, img := range frames {
		for _, plane := range []struct {
			pix    []byte
			stride int
			w, h   int
		}{
			{img.Y, img.YStride, w, h},
			{img.Cb, img.CStride, cw, ch},
			{img.Cr, img.CStride, cw, ch},
		} {
			for y := 0; y < plane.h; y++ {
				if _, err := f.Write(plane.pix[y*plane.stride : y*plane.stride+plane.w]); err != nil {
					t.Fatalf("write raw frame: %v", err)
				}
			}
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close raw file: %v", err)
	}

	// Encode to JPEGs, preserving the subsampling.
	pattern := filepath.Join(dir, "frame_%04d.jpg")
	runFF(t, ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", pixFmt, "-s", fmt.Sprintf("%dx%d", w, h),
		"-i", rawPath,
		"-c:v", "mjpeg", "-q:v", "3", "-start_number", "0", pattern)

	jpegs := make([][]byte, len(frames))
	for i := range jpegs {
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("frame_%04d.jpg", i)))
		if err != nil {
			t.Fatalf("read encoded frame %d: %v", i, err)
		}
		jpegs[i] = data
	}
	return jpegs
}

// runFFmpeg runs an ffmpeg/ffprobe command and fails the test on error.
func runFF(t *testing.T, bin string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", bin, strings.Join(args, " "), err, out)
	}
	return out
}

// decodeToRGB24 decodes a video/image sequence with ffmpeg to raw RGB24.
func decodeToRGB24(t *testing.T, ffmpeg, input string, extraInArgs ...string) []byte {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error"}
	args = append(args, extraInArgs...)
	args = append(args, "-i", input,
		"-map", "0:v:0", "-fps_mode", "passthrough",
		"-pix_fmt", "rgb24", "-f", "rawvideo", "-")
	out := runFF(t, ffmpeg, args...)
	return out
}

// frameHashes splits raw RGB24 video into frames and hashes each.
func frameHashes(t *testing.T, raw []byte, width, height int) [][sha256.Size]byte {
	t.Helper()
	frameSize := width * height * 3
	if len(raw) == 0 || len(raw)%frameSize != 0 {
		t.Fatalf("decoded size %d is not a multiple of frame size %d", len(raw), frameSize)
	}
	hashes := make([][sha256.Size]byte, len(raw)/frameSize)
	for i := range hashes {
		hashes[i] = sha256.Sum256(raw[i*frameSize : (i+1)*frameSize])
	}
	return hashes
}

// checkFrameChunks walks the movi list of the AVI file and verifies that
// each frame is stored in a "00dc" chunk whose size matches the written
// JPEG data. This pins the container structure that ffmpeg's demuxer
// tolerates deviations in (e.g. a "00db" tag still decodes) but stricter
// players may not.
func checkFrameChunks(t *testing.T, aviPath string, written [][]byte) {
	t.Helper()
	data, err := os.ReadFile(aviPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	movi := bytes.Index(data, []byte("movi"))
	if movi < 0 {
		t.Fatal("no movi list found")
	}
	pos := movi + 4
	for i, jpegData := range written {
		if pos+8 > len(data) {
			t.Fatalf("frame %d: truncated chunk header", i)
		}
		if id := string(data[pos : pos+4]); id != "00dc" {
			t.Fatalf("frame %d: chunk id = %q, want %q", i, id, "00dc")
		}
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		if size != len(jpegData) {
			t.Fatalf("frame %d: chunk size = %d, want %d", i, size, len(jpegData))
		}
		if !bytes.Equal(data[pos+8:pos+8+size], jpegData) {
			t.Fatalf("frame %d: chunk payload differs from written JPEG", i)
		}
		pos += 8 + size
		if size&1 != 0 {
			pos++ // chunks are padded to even sizes
		}
	}
	// All frames consumed: the index chunk must follow.
	if pos+4 > len(data) || string(data[pos:pos+4]) != "idx1" {
		t.Fatalf("expected idx1 chunk after %d frames at offset %d", len(written), pos)
	}
}

// probePixFmt returns the pixel format ffmpeg reports for the AVI's video stream.
func probePixFmt(t *testing.T, ffprobe, avi string) string {
	t.Helper()
	out := runFF(t, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=pix_fmt", "-of", "csv=p=0", avi)
	return strings.TrimSpace(string(out))
}

// probeFrameCount returns the frame count ffprobe reports for the AVI's
// video stream (read from the container/stream headers).
func probeFrameCount(t *testing.T, ffprobe, avi string) string {
	t.Helper()
	out := runFF(t, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=nb_frames", "-of", "csv=p=0", avi)
	return strings.TrimSpace(string(out))
}

// decodeFrameAt decodes the single frame presented at the given time (in
// seconds) using input seeking, which exercises the AVI index (idx1).
func decodeFrameAt(t *testing.T, ffmpeg, avi string, seekSec float64, width, height int) [sha256.Size]byte {
	t.Helper()
	out := runFF(t, ffmpeg, "-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatFloat(seekSec, 'f', -1, 64),
		"-i", avi, "-map", "0:v:0", "-frames:v", "1",
		"-pix_fmt", "rgb24", "-f", "rawvideo", "-")
	if want := width * height * 3; len(out) != want {
		t.Fatalf("seek to %vs: decoded %d bytes, want one frame (%d bytes)", seekSec, len(out), want)
	}
	return sha256.Sum256(out)
}

// wantPixFmt is the pixel format ffmpeg must report for each subsampling.
var wantPixFmt = map[image.YCbCrSubsampleRatio]string{
	image.YCbCrSubsampleRatio420: "yuvj420p",
	image.YCbCrSubsampleRatio422: "yuvj422p",
	image.YCbCrSubsampleRatio444: "yuvj444p",
}

func TestFFmpegInterop(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found in PATH, skipping interop test")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found in PATH, skipping interop test")
	}

	for _, tc := range interopCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			// Generate the fixed source frame sequence and encode it to
			// JPEG. 4:2:0 uses Go's image/jpeg (the documented usage of
			// this package); other subsamplings use ffmpeg's mjpeg encoder
			// as Go's encoder only supports 4:2:0.
			srcFrames := make([]*image.YCbCr, numSrcFrames)
			for i := range srcFrames {
				srcFrames[i] = makeSrcFrame(tc.ratio, tc.width, tc.height, i)
			}
			var srcJPEGs [][]byte
			if tc.ratio == image.YCbCrSubsampleRatio420 {
				srcJPEGs = make([][]byte, numSrcFrames)
				for i, f := range srcFrames {
					srcJPEGs[i] = encodeJPEG(t, f)
				}
			} else {
				srcJPEGs = encodeJPEGsWithFFmpeg(t, ffmpeg, dir, srcFrames)
			}

			// Write the AVI with the (possibly duplicated) frame sequence.
			var written [][]byte // JPEG data of each written AVI frame, in order
			aviPath := filepath.Join(dir, "out.avi")
			aw, err := New(aviPath, int32(tc.width), int32(tc.height), tc.fps)
			if err != nil {
				t.Fatalf("mjpeg.New: %v", err)
			}
			for i := 0; i < numSrcFrames; i++ {
				n := 1
				if tc.repeats != nil {
					n = tc.repeats[i]
				}
				for j := 0; j < n; j++ {
					if err := aw.AddFrame(srcJPEGs[i]); err != nil {
						t.Fatalf("AddFrame(%d): %v", i, err)
					}
					written = append(written, srcJPEGs[i])
				}
			}
			if err := aw.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// The container structure must match the written frames.
			checkFrameChunks(t, aviPath, written)

			// The AVI's video stream must carry the subsampling we encoded.
			if want := wantPixFmt[tc.ratio]; want != "" {
				if got := probePixFmt(t, ffprobe, aviPath); got != want {
					t.Errorf("pix_fmt = %q, want %q", got, want)
				}
			}

			// The container headers must report the exact frame count.
			if got, want := probeFrameCount(t, ffprobe, aviPath), strconv.Itoa(len(written)); got != want {
				t.Errorf("nb_frames = %q, want %s", got, want)
			}

			// Reference: decode each written JPEG frame with ffmpeg.
			// Frames are written to files and read back with the image2
			// demuxer, preserving order.
			for i, data := range written {
				p := filepath.Join(dir, fmt.Sprintf("ref_%04d.jpg", i))
				if err := os.WriteFile(p, data, 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}
			refRaw := decodeToRGB24(t, ffmpeg, filepath.Join(dir, "ref_%04d.jpg"),
				"-start_number", "0", "-framerate", "1", "-f", "image2")
			refHashes := frameHashes(t, refRaw, tc.width, tc.height)

			// Decode the AVI with ffmpeg.
			aviRaw := decodeToRGB24(t, ffmpeg, aviPath)
			aviHashes := frameHashes(t, aviRaw, tc.width, tc.height)

			// Frame count must match exactly (no dropped/duplicated frames).
			if len(aviHashes) != len(refHashes) {
				t.Fatalf("decoded %d frames from AVI, want %d", len(aviHashes), len(refHashes))
			}

			// Every frame must decode bit-identically to its source JPEG.
			for i := range refHashes {
				if aviHashes[i] != refHashes[i] {
					t.Errorf("frame %d: hash mismatch\n  AVI: %x\n  ref: %x",
						i, aviHashes[i], refHashes[i])
				}
			}

			// Random access via the AVI index: seeking into a frame's
			// display interval must yield exactly that frame. (All frames
			// are keyframes, so seeks land exactly.) The seek target is
			// placed 1/4 into the interval: ffmpeg rounds the target to
			// the stream timebase (1/fps), so this lands unambiguously on
			// the intended frame regardless of rounding direction.
			n := len(refHashes)
			for _, idx := range []int{0, n / 2, n - 1} {
				seek := (float64(idx) + 0.25) / float64(tc.fps)
				got := decodeFrameAt(t, ffmpeg, aviPath, seek, tc.width, tc.height)
				if got != refHashes[idx] {
					t.Errorf("seek to frame %d (t=%.4fs): hash mismatch\n  got:  %x\n  want: %x",
						idx, seek, got, refHashes[idx])
				}
			}
		})
	}
}
