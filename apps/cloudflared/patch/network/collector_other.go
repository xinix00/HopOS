//go:build !darwin && !linux && !windows

// Deze fallback hoort eigenlijk upstream: cloudflared's diagnostic/network
// heeft alleen een collector voor darwin/linux (shelt uit naar traceroute) en
// windows. Op elk ander platform — HopOS bijvoorbeeld, waar de Go-runtime hét
// OS is en er geen traceroute-binary of os/exec bestaat — mist het type en
// compileert het pakket niet. Zie tools/prepare-cloudflared.sh.

package diagnostic

import (
	"context"
	"errors"
)

// NetworkCollectorImpl meldt netjes dat een traceroute hier niet kan, i.p.v.
// het pakket onbouwbaar te laten zijn.
type NetworkCollectorImpl struct{}

func (tracer *NetworkCollectorImpl) Collect(_ context.Context, _ TraceOptions) ([]*Hop, string, error) {
	return nil, "", errors.New("traceroute is not available on this platform")
}
