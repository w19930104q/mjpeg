package mjpeg

import (
	"encoding/binary"
	"errors"
	"io"
	"math/bits"
	"os"
	"time"
)

var (
	// ErrNotAVI is returned by Inspect when the source does not start with
	// a RIFF/AVI container.
	ErrNotAVI = errors.New("Not an AVI file: missing RIFF/AVI signature")

	// ErrFrameIndex is returned by Info.FrameInterval when the requested
	// frame index is out of range.
	ErrFrameIndex = errors.New("Frame index out of range")

	// errCorrupt signals a structurally invalid or truncated AVI file.
	errCorrupt = errors.New("Corrupt or incomplete AVI file")
)

// Info describes the timing and geometry information read back from an AVI
// file. All timing information is exact rational data taken from the stream
// header (dwRate/dwScale); the integer-microsecond field UsPerFrame is only
// reported for completeness and is never used as a timing source.
type Info struct {
	// Width and Height are the video dimensions in pixels.
	Width int32
	// Height is the video height in pixels.
	Height int32

	// UsPerFrame is the avih.dwMicroSecPerFrame value: the frame delay in
	// whole microseconds. Fractional microseconds cannot be represented in
	// this field, so for non-integer frame rates it is an approximation.
	UsPerFrame uint32

	// RateNum and RateDen are the stream timebase as written in the stream
	// header (dwRate and dwScale). One timebase tick lasts RateDen/RateNum
	// seconds, and the constant frame rate is RateNum/RateDen frames/second.
	RateNum uint32
	RateDen uint32

	// Frames is the number of video frames (avih.dwTotalFrames).
	Frames int

	// StreamLength is the stream length in timebase ticks (strh.dwLength):
	// the frame count for constant frame rate files, or the sum of all
	// frame durations for variable frame rate files.
	StreamLength uint32

	// FrameDurations holds the duration of each frame in timebase ticks
	// when the file is variable frame rate (read from the "vfrd" chunk).
	// It is nil for constant frame rate files.
	FrameDurations []uint32
}

// VFR reports whether the file carries per-frame durations (variable frame
// rate).
func (i *Info) VFR() bool {
	return len(i.FrameDurations) > 0
}

// FPS returns the exact constant frame rate in frames per second. For
// variable frame rate files it returns the timebase rate, which is not the
// average frame rate; use Info.Duration and Info.FrameInterval for exact
// timing in that case.
func (i *Info) FPS() Rational {
	return Rational{Num: uint64(i.RateNum), Den: uint64(i.RateDen)}
}

// Duration returns the exact video duration in seconds as a rational number.
// For constant frame rate files it is Frames * RateDen / RateNum; for
// variable frame rate files the per-frame durations are summed.
func (i *Info) Duration() Rational {
	var ticks uint64
	if i.VFR() {
		for _, d := range i.FrameDurations {
			ticks += uint64(d)
		}
	} else {
		ticks = uint64(i.Frames)
	}
	return reduce(ticks*uint64(i.RateDen), uint64(i.RateNum))
}

// FrameInterval returns the exact on-screen duration of the frame with the
// given zero-based index, in seconds. For constant frame rate files every
// frame has the same duration; for variable frame rate files the duration
// stored for that frame is returned.
func (i *Info) FrameInterval(index int) (Rational, error) {
	var ticks uint64 = 1
	if i.VFR() {
		if index < 0 || index >= len(i.FrameDurations) {
			return Rational{}, ErrFrameIndex
		}
		ticks = uint64(i.FrameDurations[index])
	} else if index < 0 || index >= i.Frames {
		return Rational{}, ErrFrameIndex
	}
	return reduce(ticks*uint64(i.RateDen), uint64(i.RateNum)), nil
}

// TimeDuration converts a duration given in seconds as a rational number
// into a time.Duration. A time.Duration has nanosecond resolution; if the
// exact value does not fit, the result is saturated to the representable
// range.
func (r Rational) TimeDuration() time.Duration {
	const nanosBase = uint64(1_000_000_000)
	if r.Num == 0 || r.Den == 0 {
		return 0
	}
	// nanoseconds = Num * 1e9 / Den; use 128-bit-aware primitives so the
	// multiplication cannot overflow and saturate to the int64 range.
	red := reduce(r.Num, r.Den)
	hi, lo := bits.Mul64(red.Num, nanosBase)
	if hi >= red.Den || hi+1 == red.Den && lo >= 1<<63 {
		return time.Duration(1<<63 - 1)
	}
	nanos, _ := bits.Div64(hi, lo, red.Den)
	return time.Duration(nanos)
}

// reduce returns num/den as a reduced Rational.
func reduce(num, den uint64) Rational {
	if den == 0 {
		return Rational{Num: num, Den: den}
	}
	g := gcd(num, den)
	return Rational{Num: num / g, Den: den / g}
}

// InspectFile opens the AVI file at the given path and returns its timing
// and geometry information.
func InspectFile(path string) (*Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Inspect(f)
}

// Inspect reads an AVI file from r and returns its timing and geometry
// information. Timing is read from the exact rational stream header fields
// and, for variable frame rate files, from the "vfrd" per-frame duration
// chunk.
func Inspect(r io.ReadSeeker) (*Info, error) {
	var sig [12]byte
	if _, err := io.ReadFull(r, sig[:]); err != nil {
		return nil, err
	}
	if string(sig[0:4]) != "RIFF" || string(sig[8:12]) != "AVI " {
		return nil, ErrNotAVI
	}

	info := &Info{}
	fileEnd, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if _, err := r.Seek(int64(len(sig)), io.SeekStart); err != nil {
		return nil, err
	}

	// parseList walks all chunks up to end. LIST chunks are descended into
	// recursively; the nesting depth of AVI files is only a few levels.
	var parseList func(end int64) error
	parseList = func(end int64) error {
		for {
			pos, err := r.Seek(0, io.SeekCurrent)
			if err != nil {
				return err
			}
			if pos >= end {
				return nil
			}
			var ch [8]byte
			if _, err := io.ReadFull(r, ch[:]); err != nil {
				return err
			}
			fourcc := string(ch[0:4])
			size := int64(binary.LittleEndian.Uint32(ch[4:8]))
			dataPos, err := r.Seek(0, io.SeekCurrent)
			if err != nil {
				return err
			}
			next := dataPos + size
			if size&0x01 != 0 {
				next++ // chunks are padded to even length
			}
			if next > end || next < dataPos {
				return errCorrupt
			}

			if fourcc == "LIST" {
				if size < 4 {
					return errCorrupt
				}
				var listType [4]byte
				if _, err := io.ReadFull(r, listType[:]); err != nil {
					return err
				}
				if err := parseList(next); err != nil {
					return err
				}
				continue
			}

			switch fourcc {
			case "avih":
				if err := parseAvih(r, dataPos, size, info); err != nil {
					return err
				}
			case "strh":
				if err := parseStrh(r, dataPos, size, info); err != nil {
					return err
				}
			case vfrdFourCC:
				if err := parseVfrd(r, dataPos, size, info); err != nil {
					return err
				}
			}

			if _, err := r.Seek(next, io.SeekStart); err != nil {
				return err
			}
		}
	}

	if err := parseList(fileEnd); err != nil {
		return nil, err
	}
	if info.RateNum == 0 || info.RateDen == 0 {
		return nil, errCorrupt
	}
	if info.VFR() && len(info.FrameDurations) != info.Frames {
		return nil, errCorrupt
	}
	return info, nil
}

// parseAvih reads the fields of the "avih" chunk.
func parseAvih(r io.ReadSeeker, dataPos, size int64, info *Info) error {
	const avihFixedSize = 40
	if size < avihFixedSize {
		return errCorrupt
	}
	buf := make([]byte, avihFixedSize)
	if _, err := r.Seek(dataPos, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	le := binary.LittleEndian
	info.UsPerFrame = le.Uint32(buf[0:4])
	info.Frames = int(le.Uint32(buf[16:20]))
	info.Width = int32(le.Uint32(buf[32:36]))
	info.Height = int32(le.Uint32(buf[36:40]))
	return nil
}

// parseStrh reads the timing fields of the first video "strh" chunk.
func parseStrh(r io.ReadSeeker, dataPos, size int64, info *Info) error {
	const strhFixedSize = 40
	if size < strhFixedSize {
		return errCorrupt
	}
	buf := make([]byte, strhFixedSize)
	if _, err := r.Seek(dataPos, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	if string(buf[0:4]) != "vids" {
		return nil // only the single video stream is written by this package
	}
	le := binary.LittleEndian
	info.RateDen = le.Uint32(buf[20:24]) // dwScale
	info.RateNum = le.Uint32(buf[24:28]) // dwRate
	info.StreamLength = le.Uint32(buf[32:36])
	return nil
}

// parseVfrd reads the per-frame tick durations of the "vfrd" chunk.
func parseVfrd(r io.ReadSeeker, dataPos, size int64, info *Info) error {
	if size%4 != 0 {
		return errCorrupt
	}
	buf := make([]byte, size)
	if _, err := r.Seek(dataPos, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	le := binary.LittleEndian
	durations := make([]uint32, size/4)
	for i := range durations {
		durations[i] = le.Uint32(buf[i*4 : i*4+4])
	}
	info.FrameDurations = durations
	return nil
}
