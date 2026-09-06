package smc

import (
	"errors"
	"testing"
)

func TestUnconfirmedCommandCannotReuseID(t *testing.T) {
	want := errors.New("unconfirmed")
	d := Dev{failed: want, msgid: 16}
	if _, err := d.Read(Key("TC0P")); err != want {
		t.Fatalf("retry: %v", err)
	}
	if d.msgid != 16 {
		t.Fatal("reused id after timeout")
	}
}
