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

## Rational (fractional) frame rates

`New` only accepts integer frame rates. For non-integer frame rates such as
29.97 FPS (NTSC) or 23.976 FPS, use `NewRational` with the exact numerator and
denominator. They are written into the AVI stream header (`dwRate`/`dwScale`),
so the rate and the duration read back exactly — the integer-microsecond
header field (`dwMicroSecPerFrame`) cannot represent these rates and is only
an approximation.

    // 29.97 FPS = 30000/1001
    aw, err := mjpeg.NewRational("test.avi", 200, 100, 30000, 1001)
    checkErr(err)
    // ... add frames with aw.AddFrame(...)
    checkErr(aw.Close())

When the denominator is 1, the produced file is byte-for-byte identical to
`New` with the same integer rate.

## Variable frame rate (VFR)

`NewVFR` creates a writer whose stream timebase is a configurable number of
ticks per second. Each frame is added with its own explicit duration in that
timebase:

    taw, err := mjpeg.NewVFR("test.avi", 200, 100, 1000) // 1 tick = 1 ms
    checkErr(err)
    checkErr(taw.AddFrameTimed(frame1, 33)) // shown for 33 ms
    checkErr(taw.AddFrameTimed(frame2, 17)) // shown for 17 ms
    checkErr(taw.Close())

The per-frame durations are stored in a `vfrd` RIFF chunk, which players that
do not support it simply skip.

## Reading timing information back

`Inspect` / `InspectFile` read the exact timing and geometry data back from an
AVI file. All timing is rational (see `Rational`), computed from the stream
header rather than the microsecond approximation:

    info, err := mjpeg.InspectFile("test.avi")
    checkErr(err)

    fmt.Println(info.FPS())         // exact frames/second, e.g. 30000/1001
    fmt.Println(info.Duration())    // exact duration in seconds
    d, err := info.FrameInterval(0) // exact on-screen duration of a frame
    _ = d
