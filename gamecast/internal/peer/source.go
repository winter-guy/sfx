package peer

import (
	"errors"
	"io"
	"log"
	"os"
	"sync"

	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// Frame is one H.264 access unit in Annex-B form (start codes included).
type Frame struct {
	Data     []byte
	Keyframe bool
}

// FrameSource is where the streamer's encoded video comes from.
//
// Today that is an Annex-B pipe (e.g. from ffmpeg). The native capture +
// NVENC/AMF/QSV/VideoToolbox encoder plugs in here later and can then honour
// RequestKeyframe immediately instead of waiting for the next GOP.
type FrameSource interface {
	NextFrame() (Frame, error)
	RequestKeyframe()
}

var startCode = []byte{0, 0, 0, 1}

// AnnexBSource splits an H.264 Annex-B byte stream into access units.
//
// A NAL's end is only known when the next start code arrives, so a piped
// stream adds about one frame (~16 ms at 60 fps) of delay. The native encoder
// integration will hand over whole frames and avoid this.
type AnnexBSource struct {
	r *h264reader.H264Reader

	buf    []byte
	hasVCL bool
	key    bool

	warnOnce sync.Once
}

func NewAnnexBSource(r io.Reader) (*AnnexBSource, error) {
	hr, err := h264reader.NewReaderWithOptions(r, h264reader.WithIncludeSEI(true))
	if err != nil {
		return nil, err
	}
	return &AnnexBSource{r: hr}, nil
}

func (s *AnnexBSource) NextFrame() (Frame, error) {
	for {
		nal, err := s.r.NextNAL()
		if err != nil {
			if errors.Is(err, io.EOF) && s.hasVCL {
				return s.flush(), nil
			}
			return Frame{}, err
		}
		isVCL := nal.UnitType == h264reader.NalUnitTypeCodedSliceNonIdr ||
			nal.UnitType == h264reader.NalUnitTypeCodedSliceIdr

		// A non-VCL NAL (AUD/SPS/PPS/SEI) or a slice with first_mb_in_slice == 0
		// after a slice means the previous access unit is complete.
		if s.hasVCL && (!isVCL || firstMbIsZero(nal.Data)) {
			f := s.flush()
			s.add(nal.Data, nal.UnitType)
			return f, nil
		}
		s.add(nal.Data, nal.UnitType)
	}
}

func (s *AnnexBSource) add(nal []byte, t h264reader.NalUnitType) {
	s.buf = append(s.buf, startCode...)
	s.buf = append(s.buf, nal...)
	switch t {
	case h264reader.NalUnitTypeCodedSliceIdr:
		s.key = true
		s.hasVCL = true
	case h264reader.NalUnitTypeCodedSliceNonIdr:
		s.hasVCL = true
	}
}

func (s *AnnexBSource) flush() Frame {
	f := Frame{Data: s.buf, Keyframe: s.key}
	s.buf, s.hasVCL, s.key = nil, false, false
	return f
}

// RequestKeyframe can't reach an external encoder through a pipe, so the
// stream relies on the encoder's GOP (keep it ~2s, see README).
func (s *AnnexBSource) RequestKeyframe() {
	s.warnOnce.Do(func() {
		log.Printf("keyframe requested, but a piped encoder can't be asked for one; waiting for next GOP")
	})
}

// first_mb_in_slice is the first ue(v) after the 1-byte NAL header;
// a leading 1 bit encodes the value 0.
func firstMbIsZero(nal []byte) bool {
	return len(nal) > 1 && nal[1]&0x80 != 0
}

// LoopingFileSource replays an Annex-B file forever (for testing without an encoder).
type LoopingFileSource struct {
	Path string

	f   *os.File
	src *AnnexBSource
}

func (l *LoopingFileSource) NextFrame() (Frame, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if l.src == nil {
			f, err := os.Open(l.Path)
			if err != nil {
				return Frame{}, err
			}
			src, err := NewAnnexBSource(f)
			if err != nil {
				f.Close()
				return Frame{}, err
			}
			l.f, l.src = f, src
		}
		fr, err := l.src.NextFrame()
		if err == nil {
			return fr, nil
		}
		l.f.Close()
		l.f, l.src = nil, nil
		if !errors.Is(err, io.EOF) {
			return Frame{}, err
		}
	}
	return Frame{}, errors.New("file contains no frames")
}

func (l *LoopingFileSource) RequestKeyframe() {}
