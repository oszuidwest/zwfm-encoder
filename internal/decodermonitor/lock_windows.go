//go:build windows

package decodermonitor

import (
	"fmt"
	"os"
)

func lockState(_ *os.File) error { return fmt.Errorf("the local MicroMPX watchdog requires Linux") }
