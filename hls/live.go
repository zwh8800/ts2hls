package hls

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/asticode/go-astits"
	"go.uber.org/zap"
)

type liveState int

const (
	liveStatePAT liveState = iota
	liveStatePMT
	liveStatePES
	liveStateEnd
)

type Live struct {
	interval time.Duration
	logger   *zap.SugaredLogger

	resp *http.Response
	dmx  *astits.Demuxer
	mx   *astits.Muxer

	state liveState
	buf   bytes.Buffer

	program *astits.PATProgram
}

func NewLive(ctx context.Context, src string, interval time.Duration) (*Live, error) {
	client := http.Client{
		Transport: &http.Transport{
			ReadBufferSize: 4 * 1024 * 1024, // 4M
		},
	}
	resp, err := client.Get(src)
	if err != nil {
		return nil, err
	}

	logger, _ := zap.NewProduction()

	l := Live{
		interval: interval,
		resp:     resp,
		dmx:      astits.NewDemuxer(ctx, resp.Body),
		state:    liveStatePAT,
		logger:   logger.Sugar(),
	}
	l.mx = astits.NewMuxer(ctx, &l.buf)

	return &l, nil
}

func (l *Live) ReadInterval() ([]byte, error) {
	var err error
loop:
	for {
		switch l.state {
		case liveStatePAT:
			err = l.pat()

		case liveStatePMT:
			err = l.pmt()

		case liveStatePES:
			err = l.pes()

		case liveStateEnd:
			break loop
		}
		if err != nil {
			return nil, err
		}
	}

	l.state = liveStatePES

	return l.getTsData(), nil
}

func (l *Live) getTsData() []byte {
	buf := l.buf.Bytes()
	data := make([]byte, len(buf))
	copy(data, buf)
	l.buf.Reset()
	return data
}

func (l *Live) pat() error {
	for {
		d, err := l.dmx.NextData()
		if err != nil {
			return err
		}

		if d.PAT == nil {
			continue
		}
		if len(d.PAT.Programs) == 0 {
			continue
		}
		l.program = d.PAT.Programs[0]

		l.state = liveStatePMT
		return nil
	}
}

func (l *Live) pmt() error {
	for {
		d, err := l.dmx.NextData()
		if err != nil {
			return err
		}

		if d.PMT == nil || d.PID != l.program.ProgramMapID {
			continue
		}

		for _, es := range d.PMT.ElementaryStreams {
			l.logger.Infof("Stream detected: PID: %d, StreamType: %s",
				es.ElementaryPID, es.StreamType.String())
			err := l.mx.AddElementaryStream(astits.PMTElementaryStream{
				ElementaryPID:               es.ElementaryPID,
				ElementaryStreamDescriptors: es.ElementaryStreamDescriptors,
				StreamType:                  es.StreamType,
			})
			if err != nil {
				return err
			}
			l.mx.SetPCRPID(es.ElementaryPID)
		}

		l.state = liveStatePES
		return nil
	}
}

func (l *Live) pes() error {
	first := true
	var startTime time.Time

	for {
		d, err := l.dmx.NextData()
		if err != nil {
			return err
		}

		if d.PES == nil {
			continue
		}

		if d.PES.Header == nil || d.PES.Header.OptionalHeader == nil || d.PES.Header.OptionalHeader.PTS == nil {
			return errors.New("pts is nil")
		}

		pts := d.PES.Header.OptionalHeader.PTS.Time()

		if first {
			startTime = pts
			_, err = l.mx.WriteTables()
			if err != nil {
				return err
			}
			first = false
		}

		_, err = l.mx.WriteData(&astits.MuxerData{
			PID: d.PID,
			PES: d.PES,
		})

		if err != nil {
			return err
		}

		if pts.After(startTime.Add(l.interval)) {
			l.state = liveStateEnd
			return nil
		}
	}

}

func (l *Live) Close() {
	_ = l.resp.Body.Close()
}
