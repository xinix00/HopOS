package rpi5

import (
	"hop-os/metal/board/appboard"
	"hop-os/metal/board/raspi"
)

// appBoard is het app-zichtbare deel van dit board (appboard.Board): precies
// wat een app-image nodig heeft om op te draaien — core-identiteit en de
// klok-offset. De HOP-bedrading (PCIe/RP1/GEM/DHCP, board.Board) leeft in
// board/rpi5/hop en komt een app-image nooit in.
type appBoard struct{}

func (appBoard) CoreID() int            { return CoreID() }
func (appBoard) SetTimerOffset(o int64) { raspi.ARM64.TimerOffset = o }

func init() { appboard.Use(appBoard{}) }
