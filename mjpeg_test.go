package mjpeg

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// testWidth, testHeight are the video dimensions used by the tests.
const testWidth, testHeight = 64, 48

// encodeJPEG encodes img as a JPEG frame.
func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	if err := jpeg.Encode(buf, img, nil); err != nil {
		t.Fatalf("jpeg.Encode() failed: %v", err)
	}
	return buf.Bytes()
}

// solidYCbCr returns a solid-color YCbCr image of the given size.
func solidYCbCr(w, h int) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for i := range img.Y {
		img.Y[i] = 128
	}
	for i := range img.Cb {
		img.Cb[i] = 128
		img.Cr[i] = 128
	}
	return img
}

// sofJPEG builds a minimal JPEG stream carrying only an SOI marker, a
// baseline SOF segment with the given dimensions and per-component sampling
// factors, and an EOI marker. Frame validation only reads up to the SOF
// segment, so this is sufficient to probe the sampling matrix.
func sofJPEG(w, h int, compSamp ...[2]int) []byte {
	n := len(compSamp)
	segLen := 2 + 6 + 3*n
	data := []byte{
		0xFF, 0xD8, // SOI
		0xFF, 0xC0, // SOF0
		byte(segLen >> 8), byte(segLen),
		8, // precision
		byte(h >> 8), byte(h),
		byte(w >> 8), byte(w),
		byte(n),
	}
	for i, s := range compSamp {
		data = append(data, byte(i+1), byte(s[0]<<4|s[1]), 0)
	}
	return append(data, 0xFF, 0xD9) // EOI
}

// TestAddFrameMatrix is the acceptance matrix of valid, malformed and
// unsupported frames: each case must either be accepted (want == nil) or
// rejected with a *FrameError wrapping the expected classification.
func TestAddFrameMatrix(t *testing.T) {
	valid := encodeJPEG(t, solidYCbCr(testWidth, testHeight))

	// A valid frame with a corrupted SOF segment length.
	badSegLen := sofJPEG(testWidth, testHeight, [2]int{2, 2}, [2]int{1, 1}, [2]int{1, 1})
	badSegLen[4], badSegLen[5] = 0x7F, 0xFF // absurd segment length

	// A valid frame whose SOF declares zero width.
	zeroWidth := sofJPEG(testWidth, testHeight, [2]int{2, 2}, [2]int{1, 1}, [2]int{1, 1})
	zeroWidth[8], zeroWidth[9] = 0, 0

	cases := []struct {
		name string
		data []byte
		want error // nil: frame must be accepted
	}{
		// Within the supported range: decoded as usual.
		{"valid YCbCr 4:2:0", valid, nil},
		{"valid 4:4:4", sofJPEG(testWidth, testHeight, [2]int{1, 1}, [2]int{1, 1}, [2]int{1, 1}), nil},
		{"valid 4:2:2", sofJPEG(testWidth, testHeight, [2]int{2, 1}, [2]int{1, 1}, [2]int{1, 1}), nil},
		{"valid 4:2:0", sofJPEG(testWidth, testHeight, [2]int{2, 2}, [2]int{1, 1}, [2]int{1, 1}), nil},
		{"valid 4:4:0", sofJPEG(testWidth, testHeight, [2]int{1, 2}, [2]int{1, 1}, [2]int{1, 1}), nil},

		// Unsupported sampling.
		{"grayscale", encodeJPEG(t, image.NewGray(image.Rect(0, 0, testWidth, testHeight))), ErrUnsupportedSampling},
		{"2 components", sofJPEG(testWidth, testHeight, [2]int{1, 1}, [2]int{1, 1}), ErrUnsupportedSampling},
		{"4 components CMYK", sofJPEG(testWidth, testHeight, [2]int{1, 1}, [2]int{1, 1}, [2]int{1, 1}, [2]int{1, 1}), ErrUnsupportedSampling},
		{"luma 3x1", sofJPEG(testWidth, testHeight, [2]int{3, 1}, [2]int{1, 1}, [2]int{1, 1}), ErrUnsupportedSampling},
		{"luma 4x4", sofJPEG(testWidth, testHeight, [2]int{4, 4}, [2]int{1, 1}, [2]int{1, 1}), ErrUnsupportedSampling},
		{"luma 0x1", sofJPEG(testWidth, testHeight, [2]int{0, 1}, [2]int{1, 1}, [2]int{1, 1}), ErrUnsupportedSampling},
		{"chroma 2x1", sofJPEG(testWidth, testHeight, [2]int{2, 2}, [2]int{2, 1}, [2]int{1, 1}), ErrUnsupportedSampling},

		// Size changes.
		{"wider frame", encodeJPEG(t, solidYCbCr(testWidth+16, testHeight)), ErrFrameSizeMismatch},
		{"taller frame", encodeJPEG(t, solidYCbCr(testWidth, testHeight+16)), ErrFrameSizeMismatch},
		{"swapped dims", encodeJPEG(t, solidYCbCr(testHeight, testWidth)), ErrFrameSizeMismatch},

		// Corrupt frames.
		{"empty", nil, ErrCorruptFrame},
		{"not a jpeg", []byte("this is not a jpeg frame at all"), ErrCorruptFrame},
		{"SOI only", []byte{0xFF, 0xD8}, ErrCorruptFrame},
		{"truncated", valid[:10], ErrCorruptFrame},
		{"EOI before SOF", []byte{0xFF, 0xD8, 0xFF, 0xD9}, ErrCorruptFrame},
		{"scan before SOF", []byte{0xFF, 0xD8, 0xFF, 0xDA, 0, 8, 1, 1, 0, 0, 0x3F, 0}, ErrCorruptFrame},
		{"bad segment length", badSegLen, ErrCorruptFrame},
		{"zero width in SOF", zeroWidth, ErrCorruptFrame},
		{"stuffed byte outside scan", []byte{0xFF, 0xD8, 0xFF, 0x00, 0xFF, 0xC0}, ErrCorruptFrame},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			avi := filepath.Join(t.TempDir(), "test.avi")
			aw, err := New(avi, testWidth, testHeight, 25)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			defer aw.Close()

			err = aw.AddFrame(c.data)
			if c.want == nil {
				if err != nil {
					t.Fatalf("AddFrame() rejected a supported frame: %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("AddFrame() error = %v, want classification %v", err, c.want)
			}
			var fe *FrameError
			if !errors.As(err, &fe) {
				t.Fatalf("AddFrame() error %v is not a *FrameError", err)
			}
			if fe.Frame != 0 {
				t.Errorf("FrameError.Frame = %d, want 0 (first frame)", fe.Frame)
			}
		})
	}
}

// TestFrameErrorIndex verifies that the reported frame index counts the
// frames successfully added so far, that a rejected frame is not written,
// and that the writer remains usable after a rejection.
func TestFrameErrorIndex(t *testing.T) {
	avi := filepath.Join(t.TempDir(), "test.avi")
	aw, err := New(avi, testWidth, testHeight, 25)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	valid := encodeJPEG(t, solidYCbCr(testWidth, testHeight))
	gray := encodeJPEG(t, image.NewGray(image.Rect(0, 0, testWidth, testHeight)))

	for i := 0; i < 3; i++ {
		if err := aw.AddFrame(valid); err != nil {
			t.Fatalf("AddFrame(valid #%d) failed: %v", i, err)
		}
	}

	err = aw.AddFrame(gray)
	var fe *FrameError
	if !errors.As(err, &fe) {
		t.Fatalf("AddFrame(gray) error = %v, want a *FrameError", err)
	}
	if !errors.Is(err, ErrUnsupportedSampling) {
		t.Errorf("AddFrame(gray) error = %v, want classification ErrUnsupportedSampling", err)
	}
	if fe.Frame != 3 {
		t.Errorf("FrameError.Frame = %d, want 3 (4th frame, 0-based)", fe.Frame)
	}

	// The writer must remain usable: one more valid frame, then close.
	if err := aw.AddFrame(valid); err != nil {
		t.Fatalf("AddFrame(valid) after rejection failed: %v", err)
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	// Exactly the 4 accepted frames must be in the file: each frame appears
	// once as a "00dc" chunk in 'movi' and once as an index entry in 'idx1'.
	data, err := os.ReadFile(avi)
	if err != nil {
		t.Fatalf("ReadFile() failed: %v", err)
	}
	if n := bytes.Count(data, []byte("00dc")); n != 2*4 {
		t.Errorf("output contains %d \"00dc\" occurrences, want %d (4 frames)", n, 2*4)
	}
}

// TestValidFramesUnchanged verifies the output semantics for supported
// frames: the AVI header, frame count fields and index reflect exactly the
// accepted frames.
func TestValidFramesUnchanged(t *testing.T) {
	avi := filepath.Join(t.TempDir(), "test.avi")
	aw, err := New(avi, testWidth, testHeight, 25)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	const frames = 5
	frame := encodeJPEG(t, solidYCbCr(testWidth, testHeight))
	for i := 0; i < frames; i++ {
		if err := aw.AddFrame(frame); err != nil {
			t.Fatalf("AddFrame() failed: %v", err)
		}
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	data, err := os.ReadFile(avi)
	if err != nil {
		t.Fatalf("ReadFile() failed: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("RIFF")) || !bytes.Contains(data[:16], []byte("AVI ")) {
		t.Error("output is not a RIFF AVI file")
	}
	if !bytes.Contains(data, []byte("idx1")) {
		t.Error("output misses the idx1 index chunk")
	}
	if n := bytes.Count(data, []byte("00dc")); n != 2*frames {
		t.Errorf("output contains %d \"00dc\" occurrences, want %d (%d frames)", n, 2*frames, frames)
	}
	// The temporary index file must be cleaned up.
	if _, err := os.Stat(avi + ".idx_"); !os.IsNotExist(err) {
		t.Errorf("temporary index file %q was not removed", avi+".idx_")
	}
}
