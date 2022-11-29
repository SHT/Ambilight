package audio

import (
	"context"
	"fmt"
	"image/color"
	"math"
	"math/cmplx"
	"sync"
	"time"
	"unsafe"

	"ledctl3/internal/client/controller/audio/mi"

	"github.com/go-ole/go-ole"
	gcolor "github.com/gookit/color"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/moutend/go-wca/pkg/wca"
	"github.com/pkg/errors"
	"github.com/sgreben/piecewiselinear"
	"gonum.org/v1/gonum/dsp/fourier"
	"gonum.org/v1/gonum/dsp/window"

	"ledctl3/internal/client/visualizer"
	"ledctl3/pkg/gradient"
)

type Visualizer struct {
	mux sync.Mutex

	leds     int
	colors   []color.Color
	segments []Segment

	events      chan visualizer.UpdateEvent
	cancel      context.CancelFunc
	childCancel context.CancelFunc
	done        chan bool
	maxLedCount int

	processing bool

	gradient   gradient.Gradient
	windowSize int
}

func (v *Visualizer) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	v.cancel = cancel
	v.done = make(chan bool)

	go func() {
		for {
			select {
			case <-ctx.Done():
				v.done <- true
				return
			default:
				var childCtx context.Context
				childCtx, v.childCancel = context.WithCancel(ctx)

				err := v.startCapture(childCtx)
				if errors.Is(err, context.Canceled) {
					return
				} else if err != nil {
					time.Sleep(1 * time.Second)
				}
			}
		}
	}()

	return nil
}

func (v *Visualizer) Events() chan visualizer.UpdateEvent {
	return v.events
}

func (v *Visualizer) Stop() error {
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}

	<-v.done

	return nil
}

func (v *Visualizer) startCapture(ctx context.Context) error {
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		return err
	}
	defer ole.CoUninitialize()

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		return err
	}
	defer mmde.Release()

	var mmd *wca.IMMDevice
	if err := mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &mmd); err != nil {
		return err
	}
	defer mmd.Release()

	var ps *wca.IPropertyStore
	if err := mmd.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return err
	}
	defer ps.Release()

	var ac *wca.IAudioClient
	if err := mmd.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
		return err
	}
	defer ac.Release()

	var ami *mi.IAudioMeterInformation
	if err := mmd.Activate(wca.IID_IAudioMeterInformation, wca.CLSCTX_ALL, nil, &ami); err != nil {
		return err
	}
	defer ami.Release()

	var wfx *wca.WAVEFORMATEX
	if err := ac.GetMixFormat(&wfx); err != nil {
		return err
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(wfx)))

	wfx.NChannels = 2 // force channels to two
	wfx.WFormatTag = 1
	wfx.WBitsPerSample = 16
	wfx.NBlockAlign = (wfx.WBitsPerSample / 8) * wfx.NChannels
	wfx.NAvgBytesPerSec = wfx.NSamplesPerSec * uint32(wfx.NBlockAlign)
	wfx.CbSize = 0

	var defaultPeriod wca.REFERENCE_TIME
	var minimumPeriod wca.REFERENCE_TIME
	var latency time.Duration
	if err := ac.GetDevicePeriod(&defaultPeriod, &minimumPeriod); err != nil {
		return err
	}
	latency = time.Duration(int(minimumPeriod) * 100)

	if err := ac.Initialize(
		wca.AUDCLNT_SHAREMODE_SHARED,
		wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK|wca.AUDCLNT_STREAMFLAGS_LOOPBACK,
		defaultPeriod, 0, wfx, nil,
	); err != nil {
		return err
	}

	audioReadyEvent := wca.CreateEventExA(0, 0, 0, wca.EVENT_MODIFY_STATE|wca.SYNCHRONIZE)
	defer wca.CloseHandle(audioReadyEvent)

	if err := ac.SetEventHandle(audioReadyEvent); err != nil {
		return err
	}

	var bufferFrameSize uint32
	if err := ac.GetBufferSize(&bufferFrameSize); err != nil {
		return err
	}

	var acc *wca.IAudioCaptureClient
	if err := ac.GetService(wca.IID_IAudioCaptureClient, &acc); err != nil {
		return err
	}
	defer acc.Release()

	fmt.Printf("Format: PCM %d bit signed integer\n", wfx.WBitsPerSample)
	fmt.Printf("Rate: %d Hz\n", wfx.NSamplesPerSec)
	fmt.Printf("Channels: %d\n", wfx.NChannels)

	fmt.Println("Default period: ", defaultPeriod)
	fmt.Println("Minimum period: ", minimumPeriod)
	fmt.Println("Latency: ", latency)

	fmt.Printf("Allocated buffer size: %d\n", bufferFrameSize)

	if err := ac.Start(); err != nil {
		return err
	}

	var offset int
	var b *byte
	var data *byte
	var availableFrameSize uint32
	var flags uint32
	var devicePosition uint64
	var qcpPosition uint64

	errorChan := make(chan error, 1)

	var isCapturing = true

loop:
	for {
		if !isCapturing {
			close(errorChan)
			break
		}
		go func() {
			errorChan <- watchEvent(ctx, audioReadyEvent)
		}()

		select {
		case <-ctx.Done():
			isCapturing = false
			<-errorChan
			break loop
		case err := <-errorChan:
			if err != nil {
				isCapturing = false
				break
			}

			if err = acc.GetBuffer(&data, &availableFrameSize, &flags, &devicePosition, &qcpPosition); err != nil {
				continue
			}

			if availableFrameSize == 0 {
				continue
			}

			start := unsafe.Pointer(data)
			if start == nil {
				continue
			}

			lim := int(availableFrameSize) * int(wfx.NBlockAlign)
			buf := make([]byte, lim)

			for n := 0; n < lim; n++ {
				b = (*byte)(unsafe.Pointer(uintptr(start) + uintptr(n)))
				buf[n] = *b
			}

			offset += lim

			samples := make([]float64, len(buf)/4)
			for i := 0; i < len(buf); i += 4 {
				v := float64(readInt32(buf[i : i+4]))
				samples = append(samples, v)
			}

			var peak float32
			err = ami.GetPeakValue(&peak)
			if err != nil {
				continue
			}

			go v.process(samples, float64(peak))

			if err = acc.ReleaseBuffer(availableFrameSize); err != nil {
				return errors.WithMessage(err, "failed to ReleaseBuffer")
			}
		}

	}

	if err := ac.Stop(); err != nil {
		return errors.Wrap(err, "failed to stop audio client")
	}

	return nil
}

func watchEvent(ctx context.Context, event uintptr) (err error) {
	errorChan := make(chan error, 1)
	go func() {
		errorChan <- eventEmitter(event)
	}()
	select {
	case err = <-errorChan:
		close(errorChan)
		return
	case <-ctx.Done():
		err = ctx.Err()
		return
	}
}

func eventEmitter(event uintptr) (err error) {
	// if err = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
	//	return
	// }
	dw := wca.WaitForSingleObject(event, wca.INFINITE)
	if dw != 0 {
		return fmt.Errorf("failed to watch event")
	}
	// ole.CoUninitialize()
	return
}

// readInt32 reads a signed integer from a byte slice. only a slice with len(4)
// should be passed. equivalent of int32(binary.LittleEndian.Uint32(b))
func readInt32(b []byte) int32 {
	return int32(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24)
}

// readInt32 reads a signed integer from a byte slice. only a slice with len(2)
// should be passed. equivalent of int16(binary.LittleEndian.Uint16(b))
func readInt16(b []byte) int16 {
	return int16(uint32(b[0]) | uint32(b[1])<<8)
}

func SpanLog(min, max float64, nPoints int) []float64 {
	X := make([]float64, nPoints)
	min, max = math.Min(max, min), math.Max(max, min)
	d := max - min
	for i := range X {
		v := min + d*(float64(i)/float64(nPoints-1))
		v = math.Pow(v, 0.5)
		X[i] = v
	}
	return X
}

func reverse[S ~[]E, E any](s S) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

var it int

func (v *Visualizer) process(samples []float64, peak float64) {
	now := time.Now()

	v.mux.Lock()
	if v.processing {
		v.mux.Unlock()
		return
	}

	v.processing = true
	v.mux.Unlock()

	defer func() {
		v.mux.Lock()
		v.processing = false
		v.mux.Unlock()
	}()

	e := 0.0
	for _, s := range samples {
		e += math.Pow(math.Abs(s), 2)
	}
	e /= math.MaxUint64

	e = math.Max(e, 0)
	e = math.Min(e, 1)

	//e = math.Sqrt(1 - math.Pow(e-1, 2))
	//e = math.Sqrt(1 - math.Pow(e-1, 2))
	//
	//e = math.Max(e, 0.5)

	fft := fourier.NewFFT(len(samples))
	coeff := fft.Coefficients(nil, window.Hamming(samples))

	freqs := []float64{}
	var maxfreq float64

	coeff = coeff[:len(coeff)/2]
	for _, c := range coeff {
		freqs = append(freqs, cmplx.Abs(c))
		if cmplx.Abs(c) > maxfreq {
			maxfreq = cmplx.Abs(c)
		}
	}

	//fmt.Print(e)
	if peak == 0 {
		for i := range freqs {
			freqs[i] = 0
		}
	} else {
		//fmt.Print(peak)
		//for i := range freqs {
		//	freqs[i] = freqs[i] * float64(peak)
		//}
	}

	//for i := range freqs {
	//	freqs[i] = freqs[i] * e
	//}

	//fmt.Print(maxidx)

	//fmt.Println(maxfreq / float64(math.MaxUint64))

	// Only keep the first half of the fft
	//freqs = freqs[:len(freqs)/2]

	for i, f := range freqs {
		norm := normalize(f, 0, maxfreq)
		freqs[i] = norm
	}

	// Scale the frequencies so that low ones are more pronounced.
	f := piecewiselinear.Function{Y: freqs}
	f.X = SpanLog(0, 1, len(f.Y))

	maxLeds := v.maxLedCount

	freqs = make([]float64, maxLeds)
	for i := 0; i < maxLeds; i++ {
		freqs[i] = f.At(float64(i) / float64(maxLeds-1))
	}

	//p2 := make([]float64, maxLeds/2)
	//copy(p2, freqs)
	//
	//reverse(freqs) // freqs or p2
	//
	//freqs = append(p2, freqs...)

	//rand.Seed(time.Now().UnixMilli() / 500)
	//rand.Shuffle(len(freqs), func(i, j int) { freqs[i], freqs[j] = freqs[j], freqs[i] })

	pix := []byte{}

	//max := math.Max(math.Min((maxfreq)/float64(math.MaxUint16)/3, 1), 0.25)

	//for i := maxLeds - 1; i >= 0; i-- {
	for i := 0; i < maxLeds; i++ {
		freq := freqs[i]

		c := v.gradient.GetInterpolatedColor(freq)
		clr, _ := colorful.MakeColor(c)
		//c := v.gradient.GetInterpolatedColor(float64(i) / float64(maxLeds-1))

		hue, sat, val := clr.Hsv()
		initval := val
		initval = initval

		val = math.Sqrt(1 - math.Pow(freq-1, 2))

		// TODO
		//hue = hue*0.9 + math.Min(e, 1)*0.1*hue

		//mult := float64(maxLeds-i) / float64(maxLeds)

		//mult *= math.Min(e, 1)
		//val2 := val*(1-mult) + val*math.Min(e, 1)

		//val = val*0.66 + val*0.33*math.Min(max, 1)
		//val = val + val*val*e*e
		//val = val * max

		//sat = sat*(1-freq) + freq*math.Min(max, 1)
		//sat2 = math.Min(sat2, 1)
		//sat = (1-sat2)*sat*0.5 + sat*0.5

		//val = val*0.5 + math.Min(e, 1)*0.5*val
		//sat = sat*0.5 + math.Min(e, 1)*0.5*sat

		//val = val*0.5 + math.Min(e, 1)*val*0.5

		//hue = math.Min(e, 1) * hue
		//sat = math.Min(e, 1) * sat

		//val = val + val*(max)
		val = math.Min(peak*5, 1) * val
		val = math.Min(val, 1)
		val = math.Max(val, 0)
		val = 0.25 + val*0.75
		//val = math.Max(val, max)

		// @@@@@@ reset
		//val = initval

		c = colorful.Hsv(hue, sat, val)

		r, g, b, _ := c.RGBA()

		// scale down specific leds cause why not
		//if i >= 300 || (i >= 0 && i < 4) || (i >= 240 && i < 244) {
		//	g = uint32(float64(g) * 0.85)
		//	b = uint32(float64(b) * 0.9)
		//}

		r = r >> 8
		g = g >> 8
		b = b >> 8

		pix = append(pix, []byte{uint8(r), uint8(g), uint8(b), 0xFF}...)
	}

	pixs = append(pixs, pix)
	if len(pixs) > v.windowSize {
		pixs = pixs[1:]
	}

	weights := []float64{}
	weightsTotal := 0.0

	for i := 0; i < len(pixs); i++ {
		// for each history item
		w := float64((i+1)*(i+1) + len(pixs)*len(pixs))

		weights = append(weights, w)
		weightsTotal += w
	}

	pix2 := make([]float64, len(pix))
	for i, p2 := range pixs {
		for j, p := range p2 {
			pix2[j] = pix2[j] + float64(p)*weights[i]
		}
	}

	pix3 := make([]float64, len(pix))
	for i, p := range pix2 {
		avg := p / weightsTotal
		pix3[i] = float64(avg)
	}

	segs := []visualizer.Segment{}

	for _, seg := range v.segments {
		length := seg.Leds * 4
		pix4 := make([]uint8, length)

		for i := 0; i < length; i += 4 {
			offset := i

			// TODO: do the mirroring beforehand (not with the 2nd part of the fft...)
			//  by limiting max to maxleds/2 and then flipping the first half into the second
			//if i >= length/2 {
			//	offset = length - 4 - i
			//}

			pix4[i] = uint8(pix3[offset])
			pix4[i+1] = uint8(pix3[offset+1])
			pix4[i+2] = uint8(pix3[offset+2])
			pix4[i+3] = uint8(pix3[offset+3])
		}

		pix := pix4[:seg.Leds*4]

		if seg.Id == 0 {
			out := "\n"
			//out := "\n"
			for i := 0; i < len(pix); i += 4 {
				out += gcolor.RGB(pix[i], pix[i+1], pix[i+2], true).Sprintf(" ")
			}
			fmt.Print(out)
		}
		it++

		segs = append(segs, visualizer.Segment{
			Id:  seg.Id,
			Pix: pix,
		})
	}

	v.events <- visualizer.UpdateEvent{
		Segments: segs,
		Duration: time.Since(now),
	}
}

// normalize scales a value from min,max to 0,1
func normalize(val, min, max float64) float64 {
	if max == min {
		return max
	}

	return (val - min) / (max - min)
}

var pixs [][]byte

func New(opts ...Option) (v *Visualizer, err error) {
	v = new(Visualizer)

	for _, opt := range opts {
		err := opt(v)
		if err != nil {
			return nil, err
		}
	}

	v.gradient, err = gradient.New(v.colors...)
	if err != nil {
		return nil, err
	}

	v.events = make(chan visualizer.UpdateEvent, len(v.segments)*8)

	return v, nil
}

type Segment struct {
	Id   int
	Leds int
}
