package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"time"
)

func runSystemMetric(name string) {
	switch name {
	case "cpu-count":
		fmt.Println(runtime.NumCPU())
	case "unix-nano":
		fmt.Println(time.Now().UnixNano())
	case "clock-ticks":
		// The arm64 Linux auxiliary vector advertises AT_CLKTCK (17), avoiding
		// assumptions about kernel tick rates or an optional getconf executable.
		body, err := os.ReadFile("/proc/self/auxv")
		if err != nil {
			fatalf("read process clock metadata: %v", err)
		}
		for offset := 0; offset+16 <= len(body); offset += 16 {
			if binary.LittleEndian.Uint64(body[offset:offset+8]) == 17 {
				ticks := binary.LittleEndian.Uint64(body[offset+8 : offset+16])
				if ticks > 0 {
					fmt.Println(ticks)
					return
				}
			}
		}
		fatalf("process clock metadata has no AT_CLKTCK")
	}
}
