package hls

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/asticode/go-astits"
	"github.com/gen2brain/aac-go"
	"github.com/gen2brain/mpeg"
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

	program      *astits.PATProgram
	isMpegAudio  bool
	mpegAudioPmt *astits.PMTElementaryStream
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

			if es.StreamType == astits.StreamTypeMPEG1Audio {
				l.isMpegAudio = true
				l.mpegAudioPmt = es

				err = l.mx.AddElementaryStream(astits.PMTElementaryStream{
					ElementaryPID: es.ElementaryPID,
					StreamType:    astits.StreamTypeADTS,
				})
			} else {
				err = l.mx.AddElementaryStream(astits.PMTElementaryStream{
					ElementaryPID:               es.ElementaryPID,
					ElementaryStreamDescriptors: es.ElementaryStreamDescriptors,
					StreamType:                  es.StreamType,
				})
			}

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

		if l.isMpegAudio && d.PID == l.mpegAudioPmt.ElementaryPID {
			readBuf, err := mpeg.NewBuffer(bytes.NewReader(d.PES.Data))
			if err != nil {
				return err
			}
			readBuf.SetLoadCallback(readBuf.LoadReaderCallback)

			decoder := mpeg.NewAudio(readBuf)
			if !decoder.HasHeader() {
				return errors.New("HasHeader: no header")
			}
			decoder.SetFormat(mpeg.AudioS16)

			writeBuf := bytes.NewBuffer(nil)
			encoder, err := aac.NewEncoder(writeBuf, &aac.Options{
				SampleRate:  decoder.Samplerate(),
				NumChannels: decoder.Channels(),
			})
			if err != nil {
				return err
			}

			decoder.Rewind()

			for {
				sample := decoder.Decode()
				if sample == nil {
					break
				}

				err := encoder.Encode(bytes.NewReader(sample.Bytes()))
				if err != nil {
					return err
				}
			}
			err = encoder.Close()
			if err != nil {
				return err
			}

			_, err = l.mx.WriteData(&astits.MuxerData{
				PID: d.PID,
				PES: &astits.PESData{
					Data: writeBuf.Bytes(),
					Header: &astits.PESHeader{OptionalHeader: &astits.PESOptionalHeader{
						DataAlignmentIndicator: true,
						PTSDTSIndicator:        astits.PTSDTSIndicatorOnlyPTS,
						PTS:                    d.PES.Header.OptionalHeader.PTS,
					}},
				},
			})
			if err != nil {
				return err
			}
		} else {
			_, err := l.mx.WriteData(&astits.MuxerData{
				PID: d.PID,
				PES: d.PES,
			})
			if err != nil {
				return err
			}
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
