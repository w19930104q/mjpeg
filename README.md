# mjpeg

![Build Status](https://github.com/icza/mjpeg/actions/workflows/go.yml/badge.svg)
[![Go Reference](https://pkg.go.dev/badge/github.com/icza/mjpeg.svg)](https://pkg.go.dev/github.com/icza/mjpeg)
[![Go Report Card](https://goreportcard.com/badge/github.com/icza/mjpeg)](https://goreportcard.com/report/github.com/icza/mjpeg)

MJPEG video writer implementation in Go.

## Examples

Let's see an example how to turn the JPEG files `1.jpg`, `2.jpg`, ..., `10.jpg` into a movie file:

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

Example to add an `image.Image` as a frame to the video:

    aw, err := mjpeg.New("test.avi", 200, 100, 2)
    checkErr(err)

    var img image.Image
    // Acquire / initialize image, e.g.:
    // img = image.NewRGBA(image.Rect(0, 0, 200, 100))

    buf := &bytes.Buffer{}
    checkErr(jpeg.Encode(buf, img, nil))
    checkErr(aw.AddFrame(buf.Bytes()))

    checkErr(aw.Close())

## Frame validation and errors

`AddFrame()` validates every frame before writing it. Rejected frames are not
added to the video, the writer remains usable, and the returned error is a
`*mjpeg.FrameError` carrying the 0-based frame index and a classification:

- `mjpeg.ErrCorruptFrame`: the data is not a well-formed JPEG frame
  (truncated, malformed segments, no frame header, ...).
- `mjpeg.ErrUnsupportedSampling`: the frame is not a 3-component YCbCr image
  with luma sampled at most 2x2 and chroma 1x1 (4:4:4 / 4:2:2 / 4:2:0).
  Grayscale and CMYK frames fall into this category.
- `mjpeg.ErrFrameSizeMismatch`: the frame dimensions differ from the video
  dimensions passed to `mjpeg.New()`.

Use `errors.Is` / `errors.As` to inspect failures:

    var fe *mjpeg.FrameError
    if errors.As(err, &fe) {
        log.Printf("frame %d rejected: %v", fe.Frame, fe.Err)
    }
