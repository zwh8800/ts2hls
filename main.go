package main

import (
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/zwh8800/ts2hls/hls"
)

const (
	contentTypeM3u8 = "application/vnd.apple.mpegurl"
	contentTypeTs   = "text/vnd.trolltech.linguist; charset=utf-8"
)

const interval = 1 * time.Second

func pprof() {
	go func() { _ = http.ListenAndServe("127.0.0.1:8000", nil) }()
}

func main() {
	pprof()
	e := echo.New()
	e.GET("/live.m3u8", liveHandler)
	e.GET("/:live/:num/live.ts", tsHandler)
	e.Logger.Fatal(e.Start(":1323"))
}

func liveHandler(c echo.Context) error {
	src := c.QueryParam("src")

	h, err := hls.NewHls(src, interval)
	if err != nil {
		return err
	}

	data, err := h.GetLive()
	if err != nil {
		return err
	}

	return c.Blob(http.StatusOK, contentTypeM3u8, []byte(data))
}

func tsHandler(c echo.Context) error {
	hashName := c.Param("live")
	num := c.Param("num")

	h := hls.GetHls(hashName)
	if h == nil {
		return c.NoContent(http.StatusNotFound)
	}

	data, ok := h.GetTs(num)
	if !ok {
		return c.NoContent(http.StatusNotFound)
	}

	return c.Blob(http.StatusOK, contentTypeTs, data)
}