package slots

import "testing"

type systemTestAddr string

func (a systemTestAddr) Network() string { return "tcp" }
func (a systemTestAddr) String() string  { return string(a) }

func TestSlotFromRemote(t *testing.T) {
	tests := []struct {
		addr string
		slot int
		ok   bool
	}{
		{"10.100.0.2:1234", 1, true},
		{"10.100.0.17:65535", 16, true},
		{"10.100.0.1:1234", 0, false}, // HOP zelf is nooit een app.
		{"10.100.1.2:1234", 0, false},
		{"192.0.2.2:1234", 0, false},
		{"[::1]:1234", 0, false},
		{"geen-adres", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got, ok := slotFromRemote(systemTestAddr(tt.addr))
			if got != tt.slot || ok != tt.ok {
				t.Fatalf("slotFromRemote(%q) = (%d, %v), want (%d, %v)", tt.addr, got, ok, tt.slot, tt.ok)
			}
		})
	}
}
