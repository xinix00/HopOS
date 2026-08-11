//go:build !linux && !darwin && !windows

// Zelfde verhaal als patch/network/collector_other.go: cloudflared's
// system-collector leest /proc (linux), sysctl (darwin) of WMI (windows). Op
// HopOS is er geen host om uit te vragen — de app IS de machine — dus deze
// fallback zegt dat, en houdt het pakket bouwbaar.

package diagnostic

import (
	"context"
	"errors"
)

type SystemCollectorImpl struct {
	version string
}

func NewSystemCollectorImpl(version string) *SystemCollectorImpl {
	return &SystemCollectorImpl{version}
}

func (collector *SystemCollectorImpl) Collect(_ context.Context) (*SystemInformation, error) {
	return nil, errors.New("system information is not available on this platform")
}
