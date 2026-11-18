package mjpeg

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// framePayloads are arbitrary frame bodies; valid JPEG data is not required
// to verify container-level timing.
var framePayloads = [][]byte{
	[]byte("FRAME-ZERO-PAYLOAD"),
	[]byte("FRAME-ONE-PAYLOAD-LONGER"),
	[]byte("F2"),
}

// maskStrn replaces the variable timestamp inside the "strn" stream-name
// chunk with zeros so two files generated at different times can be compared
// byte-for-byte. Everything else in the file must be identical.
func maskStrn(t *testing.T, b []byte) []byte {
	t.Helper()
	out := append([]byte(nil), b...)
	idx := bytes.Index(out, []byte("strn"))
	if idx < 0 {
		t.Fatal("strn chunk not found")
	}
	size := binary.LittleEndian.Uint32(out[idx+4:])
	for i := idx + 8; i < idx+8+int(size); i++ {
		out[i] = 0
	}
	return out
}

func writeCFR(t *testing.T, ctor func(path string) (AviWriter, error)) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.avi")
	aw, err := ctor(path)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	for _, f := range framePayloads {
		if err := aw.AddFrame(f); err != nil {
			t.Fatalf("AddFrame: %v", err)
		}
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	return b
}

// TestIntegerPathByteIdentical verifies that integer frame rate output is
// byte-for-byte identical to the file produced by the original integer-only
// implementation (golden file generated from git HEAD), and that NewRational
// with denominator 1 produces exactly the same bytes as New.
func TestIntegerPathByteIdentical(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "reference_fps2.avi"))
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}

	fromNew := writeCFR(t, func(path string) (AviWriter, error) {
		return New(path, 16, 8, 2)
	})
	fromRational := writeCFR(t, func(path string) (AviWriter, error) {
		return NewRational(path, 16, 8, 2, 1)
	})

	if got, want := maskStrn(t, fromNew), maskStrn(t, golden); !bytes.Equal(got, want) {
		t.Fatalf("New() output differs from golden file (new=%d golden=%d bytes)", len(got), len(want))
	}
	if !bytes.Equal(fromNew, fromRational) {
		t.Fatalf("New(2) and NewRational(2,1) must produce identical bytes")
	}

	// CFR files never carry a VFR chunk.
	if bytes.Contains(fromNew, []byte(vfrdFourCC)) {
		t.Fatal("constant frame rate file unexpectedly contains a vfrd chunk")
	}
}

// TestRationalFrameRatesAcceptance is the 29.97 / 23.976 acceptance check:
// the exact rational rate is written and read back, the duration is exact,
// and the integer-microsecond header field demonstrably drifts.
func TestRationalFrameRatesAcceptance(t *testing.T) {
	cases := []struct {
		name           string
		rateNum        uint32
		rateDen        uint32
		frames         int
		wantUsPerFrame uint32
		// wantDuration is the exact duration in seconds after "frames" frames.
		wantNum, wantDen uint64
		// driftMicrosec is how far behind the integer-us approximation falls.
		driftMicrosec uint64
	}{
		{
			name:    "29.97 NTSC (30000/1001)",
			rateNum: 30000, rateDen: 1001, frames: 300,
			wantUsPerFrame: 33366,              // 1e6*1001/30000 truncated
			wantNum:        1001, wantDen: 100, // 10.01 s exactly
			driftMicrosec: 200, // 10,010,000 us exact vs 300*33366
		},
		{
			name:    "23.976 film (24000/1001)",
			rateNum: 24000, rateDen: 1001, frames: 240,
			wantUsPerFrame: 41708, // 1e6*1001/24000 truncated
			wantNum:        1001, wantDen: 100,
			driftMicrosec: 80,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rational.avi")
			aw, err := NewRational(path, 16, 8, tc.rateNum, tc.rateDen)
			if err != nil {
				t.Fatalf("NewRational: %v", err)
			}
			for i := 0; i < tc.frames; i++ {
				if err := aw.AddFrame(framePayloads[i%len(framePayloads)]); err != nil {
					t.Fatalf("AddFrame: %v", err)
				}
			}
			if err := aw.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// Read back from the file path.
			info, err := InspectFile(path)
			if err != nil {
				t.Fatalf("InspectFile: %v", err)
			}
			if info.RateNum != tc.rateNum || info.RateDen != tc.rateDen {
				t.Fatalf("rate read back = %d/%d, want %d/%d", info.RateNum, info.RateDen, tc.rateNum, tc.rateDen)
			}
			if fps := info.FPS(); fps.Num != uint64(tc.rateNum) || fps.Den != uint64(tc.rateDen) {
				t.Fatalf("FPS() = %v, want %d/%d", fps, tc.rateNum, tc.rateDen)
			}
			if info.UsPerFrame != tc.wantUsPerFrame {
				t.Fatalf("UsPerFrame = %d, want %d", info.UsPerFrame, tc.wantUsPerFrame)
			}
			if info.Frames != tc.frames || info.StreamLength != uint32(tc.frames) {
				t.Fatalf("frames=%d streamLength=%d, want %d/%d", info.Frames, info.StreamLength, tc.frames, tc.frames)
			}
			if info.VFR() {
				t.Fatal("CFR file reported as VFR")
			}

			dur := info.Duration()
			if dur.Num != tc.wantNum || dur.Den != tc.wantDen {
				t.Fatalf("Duration() = %v, want %d/%d", dur, tc.wantNum, tc.wantDen)
			}
			// Exact duration in whole microseconds, derived from rationals.
			exactMicro := dur.Num * 1_000_000 / dur.Den
			approxMicro := uint64(tc.frames) * uint64(info.UsPerFrame)
			if exactMicro-approxMicro != tc.driftMicrosec {
				t.Fatalf("integer-us drift = %d us, want %d us", exactMicro-approxMicro, tc.driftMicrosec)
			}
			if got := dur.TimeDuration(); got != 10010*time.Millisecond {
				t.Fatalf("time.Duration = %v, want 10.01s", got)
			}

			// Every frame shares the exact rational interval 1/fps.
			wantInterval := reduce(uint64(tc.rateDen), uint64(tc.rateNum))
			for i := 0; i < tc.frames; i++ {
				got, err := info.FrameInterval(i)
				if err != nil {
					t.Fatalf("FrameInterval(%d): %v", i, err)
				}
				if got != wantInterval {
					t.Fatalf("FrameInterval(%d) = %v, want %v", i, got, wantInterval)
				}
			}
			if _, err := info.FrameInterval(tc.frames); err != ErrFrameIndex {
				t.Fatalf("out-of-range interval err = %v, want ErrFrameIndex", err)
			}

			// The same information must be reachable from an in-memory reader.
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read file: %v", err)
			}
			info2, err := Inspect(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("Inspect(reader): %v", err)
			}
			if info2.FPS() != info.FPS() || info2.Duration() != info.Duration() {
				t.Fatalf("reader-based inspect mismatch: %v/%v vs %v/%v", info2.FPS(), info2.Duration(), info.FPS(), info.Duration())
			}
		})
	}
}

// TestVFRAcceptance verifies a variable frame rate sequence round-trips
// exactly, including per-frame intervals and the total duration, and that the
// vfrd chunk is a top-level RIFF chunk placed before the index.
func TestVFRAcceptance(t *testing.T) {
	durations := []uint32{33, 17, 50, 100} // milliseconds; total 200 ms
	path := filepath.Join(t.TempDir(), "vfr.avi")

	taw, err := NewVFR(path, 16, 8, 1000) // 1 tick = 1 ms
	if err != nil {
		t.Fatalf("NewVFR: %v", err)
	}
	for i, d := range durations {
		if err := taw.AddFrameTimed(framePayloads[i%len(framePayloads)], d); err != nil {
			t.Fatalf("AddFrameTimed: %v", err)
		}
	}
	if err := taw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	vfrdPos := bytes.Index(raw, []byte(vfrdFourCC))
	idxPos := bytes.Index(raw, []byte("idx1"))
	if vfrdPos < 0 || idxPos < 0 || vfrdPos >= idxPos {
		t.Fatalf("chunk order wrong: vfrd@%d idx1@%d", vfrdPos, idxPos)
	}

	info, err := Inspect(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !info.VFR() {
		t.Fatal("file not detected as VFR")
	}
	if info.Frames != len(durations) {
		t.Fatalf("Frames = %d, want %d", info.Frames, len(durations))
	}
	if info.StreamLength != 200 {
		t.Fatalf("StreamLength = %d, want 200 ticks", info.StreamLength)
	}
	if !equalU32(info.FrameDurations, durations) {
		t.Fatalf("FrameDurations = %v, want %v", info.FrameDurations, durations)
	}
	if got := info.Duration(); got != (Rational{Num: 1, Den: 5}) {
		t.Fatalf("Duration = %v, want 1/5 s", got)
	}
	if got := info.Duration().TimeDuration(); got != 200*time.Millisecond {
		t.Fatalf("time.Duration = %v, want 200ms", got)
	}
	for i, d := range durations {
		got, err := info.FrameInterval(i)
		if err != nil {
			t.Fatalf("FrameInterval(%d): %v", i, err)
		}
		want := reduce(uint64(d), 1000)
		if got != want {
			t.Fatalf("FrameInterval(%d) = %v, want %v", i, got, want)
		}
	}

	// A second sequence on a non-millisecond timebase: tick = 1/30000 s.
	path2 := filepath.Join(t.TempDir(), "vfr2.avi")
	taw2, err := NewVFR(path2, 16, 8, 30000)
	if err != nil {
		t.Fatalf("NewVFR: %v", err)
	}
	d2 := []uint32{1001, 1001, 1000}
	for i, d := range d2 {
		if err := taw2.AddFrameTimed(framePayloads[i], d); err != nil {
			t.Fatalf("AddFrameTimed: %v", err)
		}
	}
	if err := taw2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info2, err := InspectFile(path2)
	if err != nil {
		t.Fatalf("InspectFile: %v", err)
	}
	if got := info2.Duration(); got != reduce(3002, 30000) {
		t.Fatalf("Duration = %v, want 3002/30000", got)
	}
	if got, _ := info2.FrameInterval(2); got != reduce(1000, 30000) {
		t.Fatalf("FrameInterval(2) = %v, want 1/30", got)
	}
}

// TestVFRDefaultAndRejectedAPIs covers the plain AddFrame default duration on
// VFR writers and the API misuse errors.
func TestVFRDefaultAndRejectedAPIs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vfr.avi")
	taw, err := NewVFR(path, 16, 8, 1000)
	if err != nil {
		t.Fatalf("NewVFR: %v", err)
	}
	if err := taw.AddFrame(framePayloads[0]); err != nil {
		t.Fatalf("AddFrame on VFR writer: %v", err)
	}
	if err := taw.AddFrameTimed(framePayloads[1], 0); err != ErrInvalidDuration {
		t.Fatalf("zero duration err = %v, want ErrInvalidDuration", err)
	}
	if err := taw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := InspectFile(path)
	if err != nil {
		t.Fatalf("InspectFile: %v", err)
	}
	if len(info.FrameDurations) != 1 || info.FrameDurations[0] != 1 {
		t.Fatalf("default frame duration = %v, want [1]", info.FrameDurations)
	}

	cfr, err := NewRational(filepath.Join(t.TempDir(), "cfr.avi"), 16, 8, 30000, 1001)
	if err != nil {
		t.Fatalf("NewRational: %v", err)
	}
	// AddFrameTimed on a CFR writer must be rejected (only VFR writers accept it).
	aw := cfr.(*aviWriter)
	if err := aw.AddFrameTimed(framePayloads[0], 5); err != ErrTimedFramesNotSupported {
		t.Fatalf("AddFrameTimed on CFR err = %v, want ErrTimedFramesNotSupported", err)
	}
}

// TestVFRTickOverflow verifies that a stream whose total tick count would
// exceed the 32-bit strh.dwLength field is rejected with ErrTooLarge and
// leaves no partial frame recorded.
func TestVFRTickOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vfr.avi")
	taw, err := NewVFR(path, 16, 8, 1000)
	if err != nil {
		t.Fatalf("NewVFR: %v", err)
	}
	if err := taw.AddFrameTimed(framePayloads[0], maxUint32-10); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if err := taw.AddFrameTimed(framePayloads[1], 11); err != ErrTooLarge {
		t.Fatalf("overflow err = %v, want ErrTooLarge", err)
	}
	if err := taw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := InspectFile(path)
	if err != nil {
		t.Fatalf("InspectFile: %v", err)
	}
	if info.Frames != 1 || info.StreamLength != maxUint32-10 {
		t.Fatalf("state after rejected frame: frames=%d length=%d, want 1 / %d",
			info.Frames, info.StreamLength, maxUint32-10)
	}
}

func TestConstructorValidation(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(filepath.Join(dir, "a"), 16, 8, 0); err != ErrInvalidRate {
		t.Fatalf("New fps=0 err = %v", err)
	}
	if _, err := New(filepath.Join(dir, "b"), 0, 8, 2); err != ErrInvalidDimensions {
		t.Fatalf("New width=0 err = %v", err)
	}
	if _, err := NewRational(filepath.Join(dir, "c"), 16, 8, 30000, 0); err != ErrInvalidRate {
		t.Fatalf("NewRational den=0 err = %v", err)
	}
	// Frame duration would not fit in 32-bit microseconds: 1e6*den/num > 2^32.
	if _, err := NewRational(filepath.Join(dir, "d"), 16, 8, 1, 5000); err != ErrInvalidRate {
		t.Fatalf("NewRational too-slow rate err = %v", err)
	}
	if _, err := NewVFR(filepath.Join(dir, "e"), 16, 8, 0); err != ErrInvalidRate {
		t.Fatalf("NewVFR tps=0 err = %v", err)
	}
}

func TestInspectRejectsGarbage(t *testing.T) {
	if _, err := Inspect(bytes.NewReader([]byte("not an avi file at all, just text"))); err != ErrNotAVI {
		t.Fatalf("garbage err = %v, want ErrNotAVI", err)
	}
	if _, err := Inspect(bytes.NewReader([]byte("RIFF\x00\x00\x00\x00AVI "))); err == nil {
		t.Fatal("truncated RIFF/AVI should fail")
	}
	// RIFF/AVI signature with a malformed (undersized) LIST chunk.
	malformed := []byte("RIFF\x10\x00\x00\x00AVI " + "LIST\x02\x00\x00\x00ab")
	if _, err := Inspect(bytes.NewReader(malformed)); err == nil {
		t.Fatal("undersized LIST chunk should be rejected")
	}
}

// TestGoldenFileBackwardCompatibility reads a file produced by the original
// integer-only implementation with the new inspector.
func TestGoldenFileBackwardCompatibility(t *testing.T) {
	info, err := InspectFile(filepath.Join("testdata", "reference_fps2.avi"))
	if err != nil {
		t.Fatalf("InspectFile golden: %v", err)
	}
	if info.RateNum != 2 || info.RateDen != 1 {
		t.Fatalf("rate = %d/%d, want 2/1", info.RateNum, info.RateDen)
	}
	if info.UsPerFrame != 500000 {
		t.Fatalf("UsPerFrame = %d, want 500000", info.UsPerFrame)
	}
	if info.Frames != 3 || info.StreamLength != 3 {
		t.Fatalf("frames=%d length=%d, want 3/3", info.Frames, info.StreamLength)
	}
	if got := info.Duration(); got != (Rational{Num: 3, Den: 2}) {
		t.Fatalf("Duration = %v, want 3/2 s", got)
	}
	if info.VFR() {
		t.Fatal("golden CFR file reported as VFR")
	}
}

func TestRationalHelpers(t *testing.T) {
	if got := (Rational{Num: 30000, Den: 1001}).String(); got != "30000/1001" {
		t.Fatalf("String = %q", got)
	}
	if got := (Rational{Num: 6, Den: 4}).String(); got != "3/2" {
		t.Fatalf("String reduced = %q, want 3/2", got)
	}
	if got := (Rational{Num: 8, Den: 2}).String(); got != "4" {
		t.Fatalf("String integer = %q, want 4", got)
	}
	if got := (Rational{Num: 1, Den: 5}).TimeDuration(); got != 200*time.Millisecond {
		t.Fatalf("Duration = %v, want 200ms", got)
	}
	if got := (Rational{Num: 1 << 62, Den: 1}).TimeDuration(); got != time.Duration(1<<63-1) {
		t.Fatalf("saturated Duration = %d, want max int64", got)
	}
	if got := (Rational{Num: 0, Den: 1}).TimeDuration(); got != 0 {
		t.Fatalf("zero Duration = %v", got)
	}
	f := (Rational{Num: 30000, Den: 1001}).Float64()
	if f <= 29.97 || f >= 29.971 {
		t.Fatalf("Float64 = %v, want ~29.97", f)
	}
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
