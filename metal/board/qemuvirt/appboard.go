package qemuvirt

import "hop-os/metal/board/appboard"

// appBoard is het app-zichtbare deel van dit board (appboard.Board): precies
// wat een app-image nodig heeft om op te draaien — core-identiteit en de
// klok-offset. De HOP-bedrading (drivers, PSCI-control, board.Board) leeft in
// board/qemuvirt/hop en komt een app-image nooit in.
type appBoard struct{}

func (appBoard) CoreID() int            { return CoreID() }
func (appBoard) SetTimerOffset(o int64) { ARM64.TimerOffset = o }

func init() { appboard.Use(appBoard{}) }
