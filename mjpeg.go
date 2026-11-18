/*
Package mjpeg contains an MJPEG video format writer.

# Examples

Let's see an example how to turn the JPEG files 1.jpg, 2.jpg, ..., 10.jpg into a movie file:

	checkErr := func(err error) {
	    if err != nil {
	        panic(err)
	    }
	}

	// Video size: 200x100 pixels, FPS: 2
	aw, err := mjpeg.New("test.avi", 200, 100, 2)
	checkErr(err)

	// Create a movie from images: 1.jpg, 2.jpg, ..., 10.jpg
	for i := 1; i <= 10; i++ {
	    data, err := ioutil.ReadFile(fmt.Sprintf("%d.jpg", i))
	    checkErr(err)
	    checkErr(aw.AddFrame(data))
	}

	checkErr(aw.Close())

Example to add an image.Image as a frame to the video:

	aw, err := mjpeg.New("test.avi", 200, 100, 2)
	checkErr(err)

	var img image.Image
	// Acquire / initialize image, e.g.:
	// img = image.NewRGBA(image.Rect(0, 0, 200, 100))

	buf := &bytes.Buffer{}
	checkErr(jpeg.Encode(buf, img, nil))
	checkErr(aw.AddFrame(buf.Bytes()))

	checkErr(aw.Close())

# Rational frame rates

New only accepts integer frame rates. Use NewRational for non-integer
("fractional") frame rates such as 29.97 FPS (30000/1001) or 23.976 FPS
(24000/1001). The exact rational rate is written into the AVI stream header
(dwRate/dwScale), so reading the file back yields the exact rate and duration
with no rounding error or drift:

	// 29.97 FPS, NTSC rate
	aw, err := mjpeg.NewRational("test.avi", 200, 100, 30000, 1001)
	checkErr(err)

Variable frame rate (VFR)

NewVFR creates a writer whose stream timebase is ticksPerSecond ticks/second.
Each frame carries its own explicit duration in that timebase via
TimedAviWriter.AddFrameTimed. Per-frame durations are stored in a "vfrd" RIFF
chunk and can be read back exactly with Inspect:

	taw, err := mjpeg.NewVFR("test.avi", 200, 100, 1000) // 1 tick = 1 ms
	checkErr(err)
	checkErr(taw.AddFrameTimed(frame1, 33)) // 33 ms
	checkErr(taw.AddFrameTimed(frame2, 17)) // 17 ms
	checkErr(taw.Close())
*/
package mjpeg

import (
	"encoding/binary"
	"errors"
	"io"
	"log"
	"os"
	"time"
)

const (
	// maxUint32 is the maximum value storable in a uint32 field.
	maxUint32 = 1<<32 - 1

	// vfrdFourCC is the fourCC of the chunk holding per-frame durations
	// of variable frame rate videos. It is a top-level RIFF chunk placed
	// between the "movi" LIST and the "idx1" index chunk. Its payload is a
	// sequence of uint32 little-endian values, one per frame, each being
	// the frame duration in stream timebase ticks (dwRate/dwScale seconds
	// per tick). Players that do not know this fourCC skip the chunk and
	// fall back to constant frame rate playback.
	vfrdFourCC = "vfrd"
)

var (
	// ErrTooLarge reports if more frames cannot be added,
	// else the video file would get corrupted.
	ErrTooLarge = errors.New("Video file too large")

	// errImproperUse signals improper state (due to a previous error).
	errImproperState = errors.New("Improper State")

	// ErrInvalidDimensions reports that the video width or height is not
	// a positive value.
	ErrInvalidDimensions = errors.New("Invalid video dimensions: width and height must be positive")

	// ErrInvalidRate reports that a frame rate numerator or denominator is
	// not positive, or that the resulting frame duration cannot be stored
	// in the AVI header fields.
	ErrInvalidRate = errors.New("Invalid frame rate: numerator and denominator must be positive and the frame duration must fit in 32 bits")

	// ErrInvalidDuration reports that a timed frame was added with a zero
	// tick duration.
	ErrInvalidDuration = errors.New("Invalid frame duration: duration ticks must be greater than 0")

	// ErrTimedFramesNotSupported reports that AddFrameTimed was called on a
	// constant frame rate writer (created with New or NewRational). Only
	// writers created with NewVFR accept timed frames.
	ErrTimedFramesNotSupported = errors.New("Timed frames are only supported by writers created with NewVFR")
)

// AviWriter is an *.avi video writer.
// The video codec is MJPEG.
type AviWriter interface {
	// AddFrame adds a frame from a JPEG encoded data slice.
	AddFrame(jpegData []byte) error

	// Close finalizes and closes the avi file.
	Close() error
}

// TimedAviWriter is an AviWriter for variable frame rate (VFR) videos.
// Each frame is added with its own explicit duration, expressed in the
// writer's stream timebase ticks (see NewVFR).
type TimedAviWriter interface {
	AviWriter

	// AddFrameTimed adds a frame from a JPEG encoded data slice with the
	// given duration in stream timebase ticks. durationTicks must be greater
	// than 0 and must fit in 32 bits.
	AddFrameTimed(jpegData []byte, durationTicks uint32) error
}

// Rational is an exact rational number: the value of Num/Den.
type Rational struct {
	// Num is the numerator.
	Num uint64
	// Den is the positive denominator.
	Den uint64
}

// String returns the rational as a reduced fraction "num/den"; if the value
// is a whole number, it is returned without the denominator.
func (r Rational) String() string {
	g := gcd(r.Num, r.Den)
	num, den := r.Num/g, r.Den/g
	if den == 1 {
		return itoa(num)
	}
	return itoa(num) + "/" + itoa(den)
}

// Float64 returns the rational as a float64. The rational value is exact;
// the float64 representation may be rounded.
func (r Rational) Float64() float64 {
	return float64(r.Num) / float64(r.Den)
}

// gcd returns the greatest common divisor of a and b. gcd(a, 0) returns a.
func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// itoa formats a non-negative uint64 in base 10.
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// aviWriter is the AviWriter implementation.
type aviWriter struct {
	// aviFile is the name of the file to write the result to
	aviFile string
	// width is the width of the video
	width int32
	// height is the height of the video
	height int32
	// rateNum and rateDen define the stream timebase: one tick lasts
	// rateDen/rateNum seconds (the AVI stream header stores them as
	// dwRate=rateNum and dwScale=rateDen). For constant frame rate writers
	// each frame is exactly one tick long.
	rateNum, rateDen uint32
	// usPerFrame is the frame delay in microseconds written into the main
	// AVI header (avih.dwMicroSecPerFrame). It is the best integer
	// microsecond approximation of the exact tick duration; the exact
	// timing is always carried by rateNum/rateDen.
	usPerFrame uint32
	// vfr tells whether per-frame durations are written (variable frame
	// rate mode). When true each AddFrame call must specify a duration via
	// AddFrameTimed; plain AddFrame uses a 1-tick duration.
	vfr bool

	// avif is the avi file descriptor
	avif *os.File
	// idxFile is the name of the index file
	idxFile string
	// idxf is the index file descriptor
	idxf *os.File

	// writeErr holds the last encountered write error (to avif)
	err error

	// lengthFields contains the file positions of the length fields
	// that are filled later; used as a stack (LIFO)
	lengthFields []int64

	// Position of the frames count fields
	framesCountFieldPos, framesCountFieldPos2 int64
	// Position of the MOVI chunk
	moviPos int64

	// frames is the number of frames written to the AVI file
	frames int

	// frameDurations holds the duration of each frame in stream timebase
	// ticks in VFR mode; empty for constant frame rate writers.
	frameDurations []uint32
	// totalTicks is the sum of frame durations in stream timebase ticks.
	totalTicks uint64

	// General buffers used to write int values.
	buf4, buf2 []byte
}

// New returns a new AviWriter with the given constant, integer frame rate.
// The Close() method of the AviWriter must be called to finalize the video file.
//
// The output format is identical to previous versions of this package: the
// stream header is written with dwScale=1 and dwRate=fps. For fractional frame
// rates use NewRational instead.
func New(aviFile string, width, height, fps int32) (awr AviWriter, err error) {
	if fps <= 0 {
		return nil, ErrInvalidRate
	}
	return newWriter(aviFile, width, height, uint32(fps), 1, false)
}

// NewRational returns a new constant frame rate AviWriter whose exact frame
// rate is rateNum/rateDen frames per second. Use it for non-integer frame
// rates such as 29.97 FPS (rateNum=30000, rateDen=1001) or 23.976 FPS
// (rateNum=24000, rateDen=1001).
//
// The exact rational rate is written into the AVI stream header as
// dwRate=rateNum and dwScale=rateDen, so the rate and the video duration can
// be read back exactly (see Inspect). The avih frame-delay field
// (dwMicroSecPerFrame) only holds an integer number of microseconds and is
// therefore an approximation; it is never used as the source of timing.
//
// When rateDen is 1, the produced file is byte-for-byte identical to the file
// produced by New with the same integer rate. The Close() method of the
// AviWriter must be called to finalize the video file.
func NewRational(aviFile string, width, height int32, rateNum, rateDen uint32) (awr AviWriter, err error) {
	return newWriter(aviFile, width, height, rateNum, rateDen, false)
}

// NewVFR returns a new TimedAviWriter for a variable frame rate video whose
// stream timebase is ticksPerSecond ticks per second (one tick is
// 1/ticksPerSecond seconds). Each frame is added with its own explicit
// duration in ticks via TimedAviWriter.AddFrameTimed.
//
// Per-frame durations are stored in a "vfrd" RIFF chunk and can be read back
// exactly with Inspect. For a constant frame rate video use New or
// NewRational instead. The Close() method of the writer must be called to
// finalize the video file.
func NewVFR(aviFile string, width, height int32, ticksPerSecond uint32) (awr TimedAviWriter, err error) {
	w, err := newWriter(aviFile, width, height, ticksPerSecond, 1, true)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// newWriter is the shared constructor of all AviWriter implementations.
func newWriter(aviFile string, width, height int32, rateNum, rateDen uint32, vfr bool) (awr *aviWriter, err error) {
	if width <= 0 || height <= 0 {
		return nil, ErrInvalidDimensions
	}
	if rateNum == 0 || rateDen == 0 {
		return nil, ErrInvalidRate
	}
	// Frame delay in microseconds: 1_000_000 * rateDen / rateNum.
	// It must fit the 32-bit avih.dwMicroSecPerFrame field.
	us := uint64(1000000) * uint64(rateDen) / uint64(rateNum)
	if us > maxUint32 {
		return nil, ErrInvalidRate
	}

	aw := &aviWriter{
		aviFile:      aviFile,
		width:        width,
		height:       height,
		rateNum:      rateNum,
		rateDen:      rateDen,
		usPerFrame:   uint32(us),
		vfr:          vfr,
		idxFile:      aviFile + ".idx_",
		lengthFields: make([]int64, 0, 5),
		buf4:         make([]byte, 4),
		buf2:         make([]byte, 2),
	}

	defer func() {
		if err == nil {
			return
		}
		logErr := func(e error) {
			if e != nil {
				log.Printf("Error: %v\n", e)
			}
		}
		if aw.avif != nil {
			logErr(aw.avif.Close())
			logErr(os.Remove(aviFile))
		}
		if aw.idxf != nil {
			logErr(aw.idxf.Close())
			logErr(os.Remove(aw.idxFile))
		}
	}()

	aw.avif, err = os.Create(aviFile)
	if err != nil {
		return nil, err
	}
	aw.idxf, err = os.Create(aw.idxFile)
	if err != nil {
		return nil, err
	}

	wstr, wint32, wint16, wLenF, finalizeLenF :=
		aw.writeStr, aw.writeInt32, aw.writeInt16, aw.writeLengthField, aw.finalizeLengthField

	// Write AVI header
	wstr("RIFF")                 // RIFF type
	wLenF()                      // File length (remaining bytes after this field) (nesting level 0)
	wstr("AVI ")                 // AVI signature
	wstr("LIST")                 // LIST chunk: data encoding
	wLenF()                      // Chunk length (nesting level 1)
	wstr("hdrl")                 // LIST chunk type
	wstr("avih")                 // avih sub-chunk
	wint32(0x38)                 // Sub-chunk length excluding the first 8 bytes of avih signature and size
	wint32(int32(aw.usPerFrame)) // Frame delay time in microsec (best integer approximation; exact timing is in strh dwRate/dwScale)
	wint32(0)                    // dwMaxBytesPerSec (maximum data rate of the file in bytes per second)
	wint32(0)                    // Reserved
	wint32(0x10)                 // dwFlags, 0x10 bit: AVIF_HASINDEX (the AVI file has an index chunk at the end of the file - for good performance); Windows Media Player can't even play it if index is missing!
	aw.framesCountFieldPos = aw.currentPos()
	wint32(0)      // Number of frames
	wint32(0)      // Initial frame for non-interleaved files; non interleaved files should set this to 0
	wint32(1)      // Number of streams in the video; here 1 video, no audio
	wint32(0)      // dwSuggestedBufferSize
	wint32(width)  // Image width in pixels
	wint32(height) // Image height in pixels
	wint32(0)      // Reserved
	wint32(0)
	wint32(0)
	wint32(0)

	// Write stream information
	wstr("LIST")           // LIST chunk: stream headers
	wLenF()                // Chunk size (nesting level 2)
	wstr("strl")           // LIST chunk type: stream list
	wstr("strh")           // Stream header
	wint32(56)             // Length of the strh sub-chunk
	wstr("vids")           // fccType - type of data stream - here 'vids' for video stream
	wstr("MJPG")           // MJPG for Motion JPEG
	wint32(0)              // dwFlags
	wint32(0)              // wPriority, wLanguage
	wint32(0)              // dwInitialFrames
	wint32(int32(rateDen)) // dwScale: one stream tick is rateDen/rateNum seconds
	wint32(int32(rateNum)) // dwRate, Frame rate for video streams (the actual FPS is calculated by dividing this by dwScale)
	wint32(0)              // usually zero
	aw.framesCountFieldPos2 = aw.currentPos()
	wint32(0)  // dwLength, stream length in ticks: frame count for CFR, total tick count for VFR (filled in Close)
	wint32(0)  // dwSuggestedBufferSize for reading the stream (typically, this contains a value corresponding to the largest chunk in a stream)
	wint32(-1) // dwQuality, encoding quality given by an integer between (0 and 10,000.  If set to -1, drivers use the default quality value)
	wint32(0)  // dwSampleSize, 0 means that each frame is in its own chunk
	wint16(0)  // left of rcFrame if stream has a different size than dwWidth*dwHeight(unused)
	wint16(0)  //   ..top
	wint16(0)  //   ..right
	wint16(0)  //   ..bottom
	// end of 'strh' chunk, stream format follows
	wstr("strf")               // stream format chunk
	wLenF()                    // Chunk size (nesting level 3)
	wint32(40)                 // biSize, write header size of BITMAPINFO header structure; applications should use this size to determine which BITMAPINFO header structure is being used, this size includes this biSize field
	wint32(width)              // biWidth, width in pixels
	wint32(height)             // biWidth, height in pixels (may be negative for uncompressed video to indicate vertical flip)
	wint16(1)                  // biPlanes, number of color planes in which the data is stored
	wint16(24)                 // biBitCount, number of bits per pixel #
	wstr("MJPG")               // biCompression, type of compression used (uncompressed: NO_COMPRESSION=0)
	wint32(width * height * 3) // biSizeImage (buffer size for decompressed mage) may be 0 for uncompressed data
	wint32(0)                  // biXPelsPerMeter, horizontal resolution in pixels per meter
	wint32(0)                  // biYPelsPerMeter, vertical resolution in pixels per meter
	wint32(0)                  // biClrUsed (color table size; for 8-bit only)
	wint32(0)                  // biClrImportant, specifies that the first x colors of the color table (0: all the colors are important, or, rather, their relative importance has not been computed)
	finalizeLenF()             //'strf' chunk finished (nesting level 3)

	wstr("strn") // Use 'strn' to provide a zero terminated text string describing the stream
	name := "Created with https://github.com/icza/mjpeg" +
		" at " + time.Now().Format("2006-01-02 15:04:05 MST")
	// Name must be 0-terminated and stream name length (the length of the chunk) must be even
	if len(name)&0x01 == 0 {
		name = name + " \000" // padding space plus terminating 0
	} else {
		name = name + "\000" // terminating 0
	}
	wint32(int32(len(name))) // Length of the strn sub-CHUNK (must be even)
	wstr(name)
	finalizeLenF() // LIST 'strl' finished (nesting level 2)
	finalizeLenF() // LIST 'hdrl' finished (nesting level 1)

	wstr("LIST") // The second LIST chunk, which contains the actual data
	wLenF()      // Chunk length (nesting level 1)
	aw.moviPos = aw.currentPos()
	wstr("movi") // LIST chunk type: 'movi'

	if aw.err != nil {
		return nil, aw.err
	}

	return aw, nil
}

// writeStr writes a string to the file.
func (aw *aviWriter) writeStr(s string) {
	if aw.err != nil {
		return
	}
	_, aw.err = aw.avif.WriteString(s)
}

// writeInt32 writes a 32-bit int value to the file.
func (aw *aviWriter) writeInt32(n int32) {
	if aw.err != nil {
		return
	}
	binary.LittleEndian.PutUint32(aw.buf4, uint32(n))
	_, aw.err = aw.avif.Write(aw.buf4)
}

// writeIdxInt32 writes a 32-bit int value to the index file.
func (aw *aviWriter) writeIdxInt32(n int32) {
	if aw.err != nil {
		return
	}
	binary.LittleEndian.PutUint32(aw.buf4, uint32(n))
	_, aw.err = aw.idxf.Write(aw.buf4)
}

// writeInt16 writes a 16-bit int value to the index file.
func (aw *aviWriter) writeInt16(n int16) {
	if aw.err != nil {
		return
	}
	binary.LittleEndian.PutUint16(aw.buf2, uint16(n))
	_, aw.err = aw.avif.Write(aw.buf2)
}

// writeLengthField writes an empty int field to the avi file, and saves
// the current file position as it will be filled later.
func (aw *aviWriter) writeLengthField() {
	if aw.err != nil {
		return
	}
	pos := aw.currentPos()
	aw.lengthFields = append(aw.lengthFields, pos)
	aw.writeInt32(0)
}

// finalizeLengthField finalizes the last length field.
func (aw *aviWriter) finalizeLengthField() {
	if aw.err != nil {
		return
	}
	pos := aw.currentPos()
	if aw.err != nil {
		return
	}

	numLenFs := len(aw.lengthFields)
	if numLenFs == 0 {
		aw.err = errImproperState
		return
	}
	aw.seek(aw.lengthFields[numLenFs-1], 0)
	aw.lengthFields = aw.lengthFields[:numLenFs-1]
	aw.writeInt32(int32(pos - aw.currentPos() - 4))

	// Seek "back" but align to a 2-byte boundary
	if pos&0x01 != 0 {
		pos++
	}
	aw.seek(pos, 0)
}

// seek seeks the AVI file.
func (aw *aviWriter) seek(offset int64, whence int) (pos int64) {
	if aw.err != nil {
		return
	}
	pos, aw.err = aw.avif.Seek(offset, whence)
	return
}

// currentPos returns the current file position of the AVI file.
func (aw *aviWriter) currentPos() int64 {
	return aw.seek(0, 1) // Seek relative to current pos
}

// AddFrame implements AviWriter.AddFrame().
// ErrTooLarge is returned if the vide file is too large and would get corrupted
// if the given image would be added. The file limit is about 4GB.
//
// On a variable frame rate writer (see NewVFR) the frame gets a default
// duration of one stream timebase tick; prefer AddFrameTimed there.
func (aw *aviWriter) AddFrame(jpegData []byte) error {
	return aw.addFrame(jpegData, 1)
}

// AddFrameTimed implements TimedAviWriter.AddFrameTimed().
// The duration is given in stream timebase ticks (see NewVFR).
func (aw *aviWriter) AddFrameTimed(jpegData []byte, durationTicks uint32) error {
	if !aw.vfr {
		return ErrTimedFramesNotSupported
	}
	if durationTicks == 0 {
		return ErrInvalidDuration
	}
	return aw.addFrame(jpegData, durationTicks)
}

// addFrame writes one frame with the given duration in stream timebase ticks.
func (aw *aviWriter) addFrame(jpegData []byte, durationTicks uint32) error {
	framePos := aw.currentPos()
	// Pointers in AVI are 32 bit. Do not write beyond that else the whole AVI file will be corrupted (not playable).
	// Index entry size: 16 bytes (for each frame)
	if framePos+int64(len(jpegData))+int64(aw.frames*16) > 4200000000 { // 2^32 = 4 294 967 296
		return ErrTooLarge
	}
	// The 32-bit strh.dwLength field stores the total stream length in
	// ticks, so the accumulated VFR tick count must fit in 32 bits.
	if aw.vfr && aw.totalTicks+uint64(durationTicks) > maxUint32 {
		return ErrTooLarge
	}

	aw.frames++
	if aw.vfr {
		aw.frameDurations = append(aw.frameDurations, durationTicks)
		aw.totalTicks += uint64(durationTicks)
	}

	aw.writeInt32(0x63643030) // "00dc" compressed frame
	aw.writeLengthField()     // Chunk length (nesting level 2)
	if aw.err == nil {
		_, aw.err = aw.avif.Write(jpegData)
	}
	aw.finalizeLengthField() // "00dc" chunk finished (nesting level 2)

	// Write index data
	aw.writeIdxInt32(0x63643030)                   // "00dc" compressed frame
	aw.writeIdxInt32(0x10)                         // flags: select AVIIF_KEYFRAME (The flag indicates key frames in the video sequence. Key frames do not need previous video information to be decompressed.)
	aw.writeIdxInt32(int32(framePos - aw.moviPos)) // offset to the chunk, offset can be relative to file start or 'movi'
	aw.writeIdxInt32(int32(len(jpegData)))         // length of the chunk

	return aw.err
}

// Close implements AviWriter.Close().
func (aw *aviWriter) Close() (err error) {
	defer func() {
		aw.avif.Close()
		aw.idxf.Close()
		os.Remove(aw.idxFile)
	}()

	aw.finalizeLengthField() // LIST 'movi' finished (nesting level 1)

	// For variable frame rate videos write the per-frame durations as a
	// top-level "vfrd" chunk: a sequence of uint32 little-endian tick
	// counts, one per frame. Unknown players skip it and fall back to the
	// constant frame rate described by the stream header.
	if aw.vfr {
		aw.writeStr(vfrdFourCC)
		aw.writeInt32(int32(len(aw.frameDurations) * 4)) // payload length is always even
		if aw.err == nil {
			buf := make([]byte, 4)
			for _, d := range aw.frameDurations {
				binary.LittleEndian.PutUint32(buf, d)
				_, aw.err = aw.avif.Write(buf)
				if aw.err != nil {
					break
				}
			}
		}
	}

	// Write index
	aw.writeStr("idx1") // idx1 chunk
	var idxLength int64
	if aw.err == nil {
		idxLength, aw.err = aw.idxf.Seek(0, 1) // Seek relative to current pos
	}
	aw.writeInt32(int32(idxLength)) // Chunk length (we know its size, no need to use writeLengthField() and finalizeLengthField() pair)
	// Copy temporary index data
	if aw.err == nil {
		_, aw.err = aw.idxf.Seek(0, 0)
	}
	if aw.err == nil {
		_, aw.err = io.Copy(aw.avif, aw.idxf)
	}

	pos := aw.currentPos()
	aw.seek(aw.framesCountFieldPos, 0)
	aw.writeInt32(int32(aw.frames))
	aw.seek(aw.framesCountFieldPos2, 0)
	if aw.vfr {
		// dwLength is the stream length in ticks: sum of frame durations.
		aw.writeInt32(int32(aw.totalTicks))
	} else {
		aw.writeInt32(int32(aw.frames))
	}
	aw.seek(pos, 0)

	aw.finalizeLengthField() // 'RIFF' File finished (nesting level 0)

	return aw.err
}
