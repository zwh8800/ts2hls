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

const audioInterval = 100 * time.Millisecond

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

	isMpegAudio  bool
	mpegAudioPmt *astits.PMTElementaryStream
	mpegMp2Buf   *mpeg.Buffer
	mpegDecoder  *mpeg.Audio
	mpegEncoder  *aac.Encoder
	mpegPcmBuf   *bytes.Buffer
	mpegAACBuf   *bytes.Buffer
	mpegLastTime *astits.ClockReference
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

				l.mpegMp2Buf, err = mpeg.NewBuffer(nil)
				if err != nil {
					return err
				}

				l.mpegDecoder = mpeg.NewAudio(l.mpegMp2Buf)
				l.mpegDecoder.SetFormat(mpeg.AudioS16)
				l.mpegPcmBuf = bytes.NewBuffer(nil)
				l.mpegAACBuf = bytes.NewBuffer(nil)

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

			if es.StreamType.IsVideo() {
				l.mx.SetPCRPID(es.ElementaryPID)
			}
		}

		l.state = liveStatePES
		return nil
	}
}

func (l *Live) pes() error {
	var startTime *astits.ClockReference

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

		pts := d.PES.Header.OptionalHeader.PTS

		if startTime == nil {
			startTime = pts
			_, err = l.mx.WriteTables()
			if err != nil {
				return err
			}
		}

		if l.isMpegAudio &&
			d.PID == l.mpegAudioPmt.ElementaryPID { // mpeg 音频数据 解码并存储

			// 1. 记录音频流第一次时间戳
			if l.mpegLastTime == nil {
				l.mpegLastTime = pts
			}

			// 2. 解码并缓存
			l.decodeAudio(d)

			// 3. 判断距离上次编码，时间是否超过 audioInterval，超过则编码并输出
			if pts.Time().After(l.mpegLastTime.Time().Add(audioInterval)) {
				err := l.encodeAudio()
				if err != nil {
					return err
				}
				l.mpegLastTime = nil
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

		if pts.Time().After(startTime.Time().Add(l.interval)) {
			if l.isMpegAudio {
				err := l.encodeAudio()
				if err != nil {
					return err
				}
			}

			l.state = liveStateEnd
			return nil
		}
	}

}

func (l *Live) decodeAudio(d *astits.DemuxerData) {
	l.mpegMp2Buf.Write(d.PES.Data)
	for {
		sample := l.mpegDecoder.Decode()
		if sample == nil {
			break
		}

		l.mpegPcmBuf.Write(sample.Bytes())
	}
}

func (l *Live) encodeAudio() error {
	err := l.checkEncoder()
	if err != nil {
		return err
	}

	l.mpegAACBuf.Reset() // 重置输出

	err = l.mpegEncoder.Encode(l.mpegPcmBuf) // 编码（输入->输出）
	if err != nil {
		return err
	}

	l.mpegPcmBuf.Reset() // 重置输入

	_, err = l.mx.WriteData(&astits.MuxerData{
		PID: l.mpegAudioPmt.ElementaryPID,
		PES: &astits.PESData{
			Data: l.mpegAACBuf.Bytes(),
			Header: &astits.PESHeader{OptionalHeader: &astits.PESOptionalHeader{
				DataAlignmentIndicator: true,
				PTSDTSIndicator:        astits.PTSDTSIndicatorOnlyPTS,
				PTS:                    l.mpegLastTime,
			}},
		},
	})
	if err != nil {
		return err
	}
	return nil
}

func (l *Live) checkEncoder() error {
	if l.mpegEncoder != nil {
		return nil

	}
	var err error
	l.mpegEncoder, err = aac.NewEncoder(l.mpegAACBuf, &aac.Options{
		SampleRate:  l.mpegDecoder.Samplerate(),
		NumChannels: l.mpegDecoder.Channels(),
		BitRate:     128000,
	})
	if err != nil {
		return err
	}
	return nil
}

func (l *Live) Close() {
	_ = l.resp.Body.Close()
}
