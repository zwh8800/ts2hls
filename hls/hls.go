package hls

import (
	"context"
	"crypto/md5"
	"fmt"
	"sync"
	"time"

	"github.com/grafov/m3u8"
	"github.com/patrickmn/go-cache"
	"go.uber.org/zap"
)

var (
	hlsCache = cache.New(3*time.Minute, 5*time.Minute)
)

func Init(hlsExpire time.Duration) {
	hlsCache = cache.New(hlsExpire, 5*time.Minute)
}

// Hls represents an hls stream
type Hls struct {
	src      string
	interval time.Duration

	hashName string

	transcoder *Transcoder
	plist      *m3u8.MediaPlaylist

	sequence int
	once     sync.Once
	first    chan struct{}
	tsCache  *cache.Cache
	logger   *zap.SugaredLogger
}

// NewHls creates a Hls and put it into hls pool
func NewHls(src string, interval time.Duration) (*Hls, error) {
	hls, ok := hlsCache.Get(hashHls(src))
	if !ok {
		var err error
		hls, err = newHls(src, interval)
		if err != nil {
			return nil, err
		}
	}
	hlsCache.Set(hashHls(src), hls, cache.DefaultExpiration) // 更新超时
	return hls.(*Hls), nil
}

// GetHls get a running Hls from hls pool by hashName
func GetHls(hashName string) *Hls {
	hls, ok := hlsCache.Get(hashName)
	if !ok {
		return nil
	}
	hlsCache.Set(hashName, hls, cache.DefaultExpiration) // 更新超时
	return hls.(*Hls)
}

func newHls(src string, interval time.Duration) (*Hls, error) {
	ctx := context.Background()

	l, err := NewTranscoder(ctx, src, interval)
	if err != nil {
		return nil, err
	}

	p, err := m3u8.NewMediaPlaylist(5, 10)
	if err != nil {
		return nil, err
	}

	logger, _ := zap.NewProduction()

	return &Hls{
		src:        src,
		interval:   interval,
		hashName:   hashHls(src),
		transcoder: l,
		plist:      p,
		sequence:   0,
		tsCache:    cache.New(1*time.Minute, 1*time.Minute),
		first:      make(chan struct{}),
		logger:     logger.Sugar(),
	}, nil
}

func hashHls(src string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(src)))
}

// GetLive returns m3u8 file data
func (h *Hls) GetLive() (string, error) {
	h.once.Do(func() {
		go h.doLive()
	})

	<-h.first // 等待第一次执行成功

	return h.plist.Encode().String(), nil
}

func (h *Hls) doLive() {
	first := true
	defer func() {
		err := recover()
		if err != nil {
			h.logger.Errorf("doLive err: %v", err)
		}
		h.Close()

		if first {
			close(h.first)
		}
	}()
	for {
		if _, ok := hlsCache.Get(h.hashName); !ok {
			h.logger.Infof("live %s expired, closing", h.src)
			return
		}

		data, err := h.transcoder.ReadInterval()
		if err != nil {
			h.logger.Errorf("ReadInterval err: %s", err.Error())
			return
		}

		key := fmt.Sprintf("./%s/%d/live.ts", h.hashName, h.sequence)

		h.tsCache.Set(key, data, cache.DefaultExpiration)

		h.plist.Slide(key, h.interval.Seconds(), "")
		h.sequence++

		if first {
			close(h.first)
			first = false
		}
	}
}

// Close stop transcoding and remove Hls from hls pool
func (h *Hls) Close() {
	defer func() { recover() }()
	hlsCache.Delete(h.hashName)
	h.transcoder.Close()
}

// GetTs returns hls segment(ts) by num
func (h *Hls) GetTs(key string) ([]byte, bool) {
	ts, ok := h.tsCache.Get(fmt.Sprintf("/%s/%s/live.ts", h.hashName, key))
	if !ok {
		return nil, ok
	}
	return ts.([]byte), ok
}

// GetHashName returns hls hash name
func (h *Hls) GetHashName() string {
	return h.hashName
}
