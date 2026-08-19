package appnet

import (
	"net"
	"testing"

	"github.com/xinix00/lean/leannet"
)

type discardDevice struct{}

func (discardDevice) Receive([]byte) (int, error) { return 0, nil }
func (discardDevice) Transmit([]byte) error       { return nil }

func TestJoinMulticastDispatchesByIPFamily(t *testing.T) {
	st := leannet.NewStack(discardDevice{}, leannet.Config{
		Prefix: 24,
		MAC:    [6]byte{0x02, 0, 0, 0, 0, 1},
		Budget: 1 << 20,
	}, 1)
	previous := current
	current = st
	t.Cleanup(func() {
		current = previous
		st.Close()
	})

	for _, group := range []net.IP{
		net.IPv4(224, 0, 0, 251),
		net.ParseIP("ff02::fb"),
	} {
		if err := JoinMulticast(group); err != nil {
			t.Errorf("JoinMulticast(%s): %v", group, err)
		}
	}
}
