//go:build !media

package slots

import "github.com/xinix00/HopOS/metal/v2/abi/hopabi"

// Buiten de media-smaak linkt kern geen codec- en geen apparaatdienst
// (codec.go, codecabi.go, devices.go). De ops van de ABI bestaan wel — een
// app is smaak-onafhankelijk — en krijgen hetzelfde antwoord als op een board
// zonder codec-ijzer of zonder drive: geen fout van de node, er is gewoon
// niets.

// codecHandles is leeg: er gaat nooit een sessie open.
type codecHandles struct{}

func (*codecHandles) shut() {}

// ReleaseCodecs heeft niets om te sluiten.
func ReleaseCodecs(int) {}

func (s *servicer) codecServe(req hopabi.Req) []byte {
	return failWith(req, hopabi.StatusError, "this node has no codec hardware")
}

// deviceServe: een lege /devices — te stat'en en te listen, verder niets.
func (s *servicer) deviceServe(req hopabi.Req, path string, maxChunk int, _ *[]byte) []byte {
	if path == DevicesDir {
		switch req.Op {
		case hopabi.OpStat:
			return ok(req, 0, nil)
		case hopabi.OpList:
			return listRespLimit(req, nil, maxChunk)
		}
	}
	return fail(req, ErrNoDevice)
}
