package applib

import (
	"context"
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/abi/hopabi"
)

// DeviceCommand doet precies één uitwisseling met een gemount apparaat
// (/devices). Hij verbindt niet opnieuw en herhaalt niets: de opdracht kan de
// drive al veranderd hebben. Status en sense zijn van SCSI, niet van het
// transport — een fout daarin is een geslaagde call.
func (a *App) DeviceCommand(ctx context.Context, path string, cdb, out, in []byte, timeout time.Duration) (int, byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, nil, err
	}
	if timeout <= 0 || timeout > hopabi.DeviceMaxTimeoutMS*time.Millisecond {
		return 0, 0, nil, fmt.Errorf("device command: invalid timeout")
	}
	if len(out) > MaxIOChunk-hopabi.DeviceCommandLen || len(in) > MaxIOChunk-hopabi.DeviceResultLen {
		return 0, 0, nil, fmt.Errorf("device command: invalid transfer length")
	}
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	if timeout <= 0 {
		return 0, 0, nil, context.DeadlineExceeded
	}
	deadline := time.Now().Add(timeout)
	a.mu.Lock()
	defer a.mu.Unlock()
	// Het wachten op a.mu telt mee: HOP krijgt alleen de tijd die er
	// nog over is.
	if err := ctx.Err(); err != nil {
		return 0, 0, nil, err
	}
	timeout = time.Until(deadline)
	if timeout <= 0 {
		return 0, 0, nil, context.DeadlineExceeded
	}
	ms := uint32((timeout + time.Millisecond - 1) / time.Millisecond)
	data, err := hopabi.EncodeDeviceCommand(hopabi.DeviceCommand{CDB: cdb, TimeoutMS: ms, InLen: uint32(len(in)), DataOut: out})
	if err != nil {
		return 0, 0, nil, err
	}
	// Twee seconden speling bovenop de opdracht zelf, zodat een antwoord van
	// HOP dat net op tijd klaar is ook nog over de draad komt.
	response, err := a.rpcNoRetryLocked(hopabi.Req{Op: hopabi.OpDeviceCommand, Path: path, Data: data}, timeout+2*time.Second)
	if err != nil {
		return 0, 0, nil, err
	}
	result, err := hopabi.DecodeDeviceResult(response.Data, len(in), len(out))
	if err != nil {
		return 0, 0, nil, err
	}
	if response.Size != uint64(result.Transferred) {
		return 0, 0, nil, fmt.Errorf("device command: inconsistent response length")
	}
	copy(in, result.Data)
	return int(result.Transferred), result.Status, append([]byte(nil), result.Sense...), nil
}
