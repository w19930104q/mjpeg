/*

Package mjpeg contains an MJPEG video format writer.

Examples

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
*/
package mjpeg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"
)

var (
	// ErrTooLarge reports if more frames cannot be added,
	// else the video file would get corrupted.
	ErrTooLarge = errors.New("Video file too large")

	// errImproperUse signals improper state (due to a previous error).
	errImproperState = errors.New("Improper State")

	// ErrCorruptFrame reports that a frame is not a well-formed JPEG image
	// (e.g. missing SOI marker, truncated or malformed segments,
	// or no frame header before the entropy-coded data).
	ErrCorruptFrame = errors.New("Corrupt JPEG frame")

	// ErrUnsupportedSampling reports that a frame uses a color / sampling
	// format MJPEG players in general cannot decode. Only 3-component
	// (YCbCr) frames are supported, with the luma component sampled at
	// most 2x2 and the chroma components sampled 1x1
	// (i.e. 4:4:4, 4:2:2 and 4:2:0). Grayscale (1 component), CMYK
	// (4 components) and exotic subsamplings are rejected.
	ErrUnsupportedSampling = errors.New("Unsupported JPEG sampling")

	// ErrFrameSizeMismatch reports that a frame's pixel dimensions differ
	// from the video dimensions passed to New().
	ErrFrameSizeMismatch = errors.New("Frame size differs from video size")
)

// FrameError describes a frame rejected by AviWriter.AddFrame().
// The frame is not added to the video and the writer remains usable.
type FrameError struct {
	// Frame is the 0-based index of the rejected frame, i.e. the number of
	// frames successfully added so far.
	Frame int
	// Err is the classification of the failure: one of ErrCorruptFrame,
	// ErrUnsupportedSampling or ErrFrameSizeMismatch.
	Err error
	// Detail optionally holds a human-readable description of the
	// offending frame parameter (e.g. the actual size or sampling).
	Detail string
}

// Error implements error.
func (e *FrameError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("mjpeg: frame %d: %v: %s", e.Frame, e.Err, e.Detail)
	}
	return fmt.Sprintf("mjpeg: frame %d: %v", e.Frame, e.Err)
}

// Unwrap returns the classification of the failure, so callers may use
// errors.Is(err, mjpeg.ErrCorruptFrame) etc.
func (e *FrameError) Unwrap() error { return e.Err }

// AviWriter is an *.avi video writer.
// The video codec is MJPEG.
type AviWriter interface {
	// AddFrame adds a frame from a JPEG encoded data slice.
	AddFrame(jpegData []byte) error

	// Close finalizes and closes the avi file.
	Close() error
}

// aviWriter is the AviWriter implementation.
type aviWriter struct {
	// aviFile is the name of the file to write the result to
	aviFile string
	// width is the width of the video
	width int32
	// height is the height of the video
	height int32
	// fps is the frames/second (the "speed") of the video
	fps int32

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

	// General buffers used to write int values.
	buf4, buf2 []byte
}

// New returns a new AviWriter.
// The Close() method of the AviWriter must be called to finalize the video file.
func New(aviFile string, width, height, fps int32) (awr AviWriter, err error) {
	aw := &aviWriter{
		aviFile:      aviFile,
		width:        width,
		height:       height,
		fps:          fps,
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
	wstr("RIFF")          // RIFF type
	wLenF()               // File length (remaining bytes after this field) (nesting level 0)
	wstr("AVI ")          // AVI signature
	wstr("LIST")          // LIST chunk: data encoding
	wLenF()               // Chunk length (nesting level 1)
	wstr("hdrl")          // LIST chunk type
	wstr("avih")          // avih sub-chunk
	wint32(0x38)          // Sub-chunk length excluding the first 8 bytes of avih signature and size
	wint32(1000000 / fps) // Frame delay time in microsec
	wint32(0)             // dwMaxBytesPerSec (maximum data rate of the file in bytes per second)
	wint32(0)             // Reserved
	wint32(0x10)          // dwFlags, 0x10 bit: AVIF_HASINDEX (the AVI file has an index chunk at the end of the file - for good performance); Windows Media Player can't even play it if index is missing!
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
	wstr("LIST") // LIST chunk: stream headers
	wLenF()      // Chunk size (nesting level 2)
	wstr("strl") // LIST chunk type: stream list
	wstr("strh") // Stream header
	wint32(56)   // Length of the strh sub-chunk
	wstr("vids") // fccType - type of data stream - here 'vids' for video stream
	wstr("MJPG") // MJPG for Motion JPEG
	wint32(0)    // dwFlags
	wint32(0)    // wPriority, wLanguage
	wint32(0)    // dwInitialFrames
	wint32(1)    // dwScale
	wint32(fps)  // dwRate, Frame rate for video streams (the actual FPS is calculated by dividing this by dwScale)
	wint32(0)    // usually zero
	aw.framesCountFieldPos2 = aw.currentPos()
	wint32(0)  // dwLength, playing time of AVI file as defined by scale and rate (set equal to the number of frames)
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
// Each frame is validated before it is written. If the JPEG data is corrupt,
// uses an unsupported sampling (e.g. grayscale or CMYK), or its dimensions
// differ from the video dimensions, a *FrameError classifying the failure
// (and carrying the frame index) is returned, the frame is not added and
// the writer remains usable.
func (aw *aviWriter) AddFrame(jpegData []byte) error {
	if err := aw.validateFrame(jpegData); err != nil {
		return err
	}

	framePos := aw.currentPos()
	// Pointers in AVI are 32 bit. Do not write beyond that else the whole AVI file will be corrupted (not playable).
	// Index entry size: 16 bytes (for each frame)
	if framePos+int64(len(jpegData))+int64(aw.frames*16) > 4200000000 { // 2^32 = 4 294 967 296
		return ErrTooLarge
	}

	aw.frames++

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

// sofInfo holds the frame parameters parsed from a JPEG SOF marker segment.
type sofInfo struct {
	width, height int
	// compSamp holds the (horizontal, vertical) sampling factor of each
	// color component, in the order they appear in the frame header.
	compSamp [][2]int
}

// scanJPEGSOF scans the JPEG marker segments of data and returns the
// parameters from the first SOF (start of frame) segment.
// It returns an error if data is not a well-formed JPEG stream up to and
// including the frame header.
func scanJPEGSOF(data []byte) (*sofInfo, error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, errors.New("missing SOI marker")
	}
	pos := 2
	for {
		// A marker is a 0xFF byte (possibly preceded by 0xFF fill bytes)
		// followed by a non-zero marker code.
		if pos >= len(data) || data[pos] != 0xFF {
			return nil, errors.New("expected marker")
		}
		for pos < len(data) && data[pos] == 0xFF {
			pos++
		}
		if pos >= len(data) {
			return nil, errors.New("truncated marker")
		}
		marker := data[pos]
		pos++
		if marker == 0x00 {
			return nil, errors.New("stuffed byte outside entropy-coded data")
		}
		if marker == 0x01 || marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) {
			continue // TEM, SOI, RSTn: markers without a length field
		}
		if marker == 0xD9 {
			return nil, errors.New("EOI before frame header")
		}
		if marker == 0xDA {
			return nil, errors.New("scan data before frame header")
		}
		if pos+2 > len(data) {
			return nil, errors.New("truncated segment length")
		}
		segLen := int(data[pos])<<8 | int(data[pos+1])
		if segLen < 2 || pos+segLen > len(data) {
			return nil, errors.New("invalid segment length")
		}
		// SOFn markers: C0-CF except DHT (C4), JPG (C8) and DAC (CC).
		isSOF := marker >= 0xC0 && marker <= 0xCF &&
			marker != 0xC4 && marker != 0xC8 && marker != 0xCC
		if !isSOF {
			pos += segLen
			continue
		}
		if segLen < 8 {
			return nil, errors.New("short frame header")
		}
		seg := data[pos+2 : pos+segLen]
		height := int(seg[1])<<8 | int(seg[2])
		width := int(seg[3])<<8 | int(seg[4])
		numComp := int(seg[5])
		if width == 0 || height == 0 {
			return nil, errors.New("zero frame dimension")
		}
		if numComp == 0 || segLen < 6+3*numComp {
			return nil, errors.New("invalid component count")
		}
		info := &sofInfo{width: width, height: height, compSamp: make([][2]int, numComp)}
		for i := 0; i < numComp; i++ {
			s := seg[6+3*i+1]
			info.compSamp[i] = [2]int{int(s >> 4), int(s & 0x0F)}
		}
		return info, nil
	}
}

// validateFrame checks that jpegData is a well-formed JPEG frame whose
// format is supported by MJPEG and whose dimensions match the video.
// It returns a *FrameError classifying the failure, or nil if the frame
// may be added.
func (aw *aviWriter) validateFrame(jpegData []byte) error {
	info, err := scanJPEGSOF(jpegData)
	if err != nil {
		return &FrameError{Frame: aw.frames, Err: ErrCorruptFrame, Detail: err.Error()}
	}
	if info.width != int(aw.width) || info.height != int(aw.height) {
		return &FrameError{
			Frame: aw.frames,
			Err:   ErrFrameSizeMismatch,
			Detail: fmt.Sprintf("frame is %dx%d, video is %dx%d",
				info.width, info.height, aw.width, aw.height),
		}
	}
	if len(info.compSamp) != 3 {
		return &FrameError{
			Frame:  aw.frames,
			Err:    ErrUnsupportedSampling,
			Detail: fmt.Sprintf("%d color components, want 3 (YCbCr)", len(info.compSamp)),
		}
	}
	for i, s := range info.compSamp {
		h, v := s[0], s[1]
		ok := h >= 1 && h <= 2 && v >= 1 && v <= 2
		if i > 0 {
			ok = h == 1 && v == 1
		}
		if !ok {
			return &FrameError{
				Frame: aw.frames,
				Err:   ErrUnsupportedSampling,
				Detail: fmt.Sprintf("component %d sampled %dx%d, want luma at most 2x2 and chroma 1x1",
					i, h, v),
			}
		}
	}
	return nil
}

// Close implements AviWriter.Close().
func (aw *aviWriter) Close() (err error) {
	defer func() {
		aw.avif.Close()
		aw.idxf.Close()
		os.Remove(aw.idxFile)
	}()

	aw.finalizeLengthField() // LIST 'movi' finished (nesting level 1)

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
	aw.writeInt32(int32(aw.frames))
	aw.seek(pos, 0)

	aw.finalizeLengthField() // 'RIFF' File finished (nesting level 0)

	return aw.err
}
