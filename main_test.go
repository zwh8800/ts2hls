package main

import (
	"fmt"
	"github.com/grafov/m3u8"
	"testing"
)

func TestM3u8(t *testing.T) {
	p, _ := m3u8.NewMediaPlaylist(3, 5)
	for i := 0; i < 10; i++ {
		p.Slide(fmt.Sprintf("test%d.ts", i), 5.0, "")
	}

	fmt.Printf("%s\n", p)
}
