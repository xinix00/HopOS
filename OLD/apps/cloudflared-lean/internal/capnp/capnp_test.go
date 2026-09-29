package capnp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// De draad-vorm is hier met de hand geschreven, dus de rondgang bouwen→lezen is
// de toets die telt: elk veld op de plek waar de gegenereerde cloudflared-code
// hem ook verwacht.
func TestRoundTrip(t *testing.T) {
	b := NewBuilder()
	root := b.Root(1, 3) // data 8, ptr 3 — zoals registerConnection's params
	root.SetUint8(0, 7)
	root.SetUint16(2, 0xbeef)
	root.SetUint32(4, 0x12345678)

	auth := root.NewStruct(0, 0, 2)
	auth.SetText(0, "9c2b680da60a658926b3fe5b3bf5f8ee")
	auth.SetData(1, []byte{1, 2, 3, 4, 5})

	root.SetData(1, []byte{0xde, 0xad, 0xbe, 0xef})

	opts := root.NewStruct(2, 1, 2)
	client := opts.NewStruct(0, 0, 4)
	client.SetData(0, []byte("id"))
	client.SetTextList(1, []string{"eerste", "tweede", "derde"})
	client.SetText(2, "v0.1.0")
	client.SetText(3, "hopos_riscv64")
	opts.SetBool(0, true)
	opts.SetUint8(1, 3)
	opts.SetUint8(2, 42)

	var buf bytes.Buffer
	if err := WriteMessage(&buf, b.Message()); err != nil {
		t.Fatal(err)
	}
	seg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	r := NewReader(seg)
	got, err := r.RootStruct()
	if err != nil {
		t.Fatal(err)
	}
	if v := got.Uint16(2); v != 0xbeef {
		t.Errorf("Uint16(2) = 0x%x, wil 0xbeef", v)
	}
	if v := got.Uint32(4); v != 0x12345678 {
		t.Errorf("Uint32(4) = 0x%x, wil 0x12345678", v)
	}
	gotAuth, err := got.StructPtr(0)
	if err != nil {
		t.Fatal(err)
	}
	if tag, _ := gotAuth.Text(0); tag != "9c2b680da60a658926b3fe5b3bf5f8ee" {
		t.Errorf("accountTag = %q", tag)
	}
	if secret, _ := gotAuth.Bytes(1, false); !bytes.Equal(secret, []byte{1, 2, 3, 4, 5}) {
		t.Errorf("tunnelSecret = %v", secret)
	}
	if id, _ := got.Bytes(1, false); !bytes.Equal(id, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Errorf("tunnelId = %v", id)
	}
	gotOpts, err := got.StructPtr(2)
	if err != nil {
		t.Fatal(err)
	}
	if !gotOpts.Bool(0) {
		t.Error("replaceExisting = false, wil true")
	}
	// compressionQuality staat op byte 1, numPreviousAttempts op byte 2 — de
	// plekken die de gegenereerde accessors noemen.
	if v := gotOpts.Uint32(0) >> 8 & 0xff; v != 3 {
		t.Errorf("compressionQuality = %d, wil 3", v)
	}
	if v := gotOpts.Uint32(0) >> 16 & 0xff; v != 42 {
		t.Errorf("numPreviousAttempts = %d, wil 42", v)
	}
	gotClient, err := gotOpts.StructPtr(0)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := gotClient.Text(2); v != "v0.1.0" {
		t.Errorf("version = %q", v)
	}
	if v, _ := gotClient.Text(3); v != "hopos_riscv64" {
		t.Errorf("arch = %q", v)
	}
}

// Een lege pointer is in Cap'n Proto de default en geen fout: velden die de
// afzender niet zette horen als "leeg" te lezen, niet als een leesfout.
func TestNullPointersAreDefaults(t *testing.T) {
	b := NewBuilder()
	root := b.Root(1, 3)
	root.SetUint8(0, 1)
	var buf bytes.Buffer
	if err := WriteMessage(&buf, b.Message()); err != nil {
		t.Fatal(err)
	}
	seg, _ := ReadMessage(&buf)
	got, err := NewReader(seg).RootStruct()
	if err != nil {
		t.Fatal(err)
	}
	child, err := got.StructPtr(1)
	if err != nil {
		t.Fatalf("lege pointer gaf een fout: %v", err)
	}
	if !child.IsNull() {
		t.Error("lege pointer niet als null gelezen")
	}
	if v := child.Uint32(0); v != 0 {
		t.Errorf("veld uit een null-struct = %d, wil 0", v)
	}
	if txt, err := got.Text(2); err != nil || txt != "" {
		t.Errorf("lege text = %q, %v", txt, err)
	}
}

// Buiten de data-sectie lezen geeft nul: dat is de regel waarmee een nieuwer
// schema oudere berichten kan lezen. Zonder dit zou een edge die een veld
// weglaat ons laten struikelen.
func TestShortStructReadsZero(t *testing.T) {
	b := NewBuilder()
	root := b.Root(1, 1) // één data-woord
	root.SetUint32(0, 5)
	var buf bytes.Buffer
	WriteMessage(&buf, b.Message())
	seg, _ := ReadMessage(&buf)
	got, _ := NewReader(seg).RootStruct()
	if v := got.Int64(8); v != 0 { // woord 2 bestaat niet
		t.Errorf("lezen voorbij de data-sectie gaf %d, wil 0", v)
	}
	if got.Bool(200) {
		t.Error("bit voorbij de data-sectie was true")
	}
}

// Bytes van het netwerk: een pointer die buiten het segment wijst moet een fout
// geven en niet in andermans geheugen lezen.
func TestRefusesPointerOutsideSegment(t *testing.T) {
	// Eén woord: de wortel-pointer wijst 100 woorden vooruit, in het niets.
	seg := make([]byte, 8)
	// struct-pointer (type 0), offset 100 woorden, 1 data-woord
	binary.LittleEndian.PutUint64(seg, uint64(100)<<2|uint64(1)<<32)
	if _, err := NewReader(seg).RootStruct(); err == nil {
		t.Error("geen fout op een pointer buiten het segment")
	}
}

// Een bericht met meer segmenten wordt geweigerd in plaats van half gelezen.
func TestRefusesMultiSegment(t *testing.T) {
	raw := []byte{1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0}
	if _, err := ReadMessage(bytes.NewReader(raw)); err == nil {
		t.Error("geen fout op een bericht met twee segmenten")
	}
}

// Het stream-formaat: aantal segmenten min één, dan de lengte in woorden. Als
// dit misgaat praat de hele registratie langs de edge heen, dus het hoort
// vastgelegd te zijn en niet alleen in een opmerking.
func TestStreamFraming(t *testing.T) {
	var buf bytes.Buffer
	seg := make([]byte, 24) // drie woorden
	if err := WriteMessage(&buf, seg); err != nil {
		t.Fatal(err)
	}
	head := buf.Bytes()[:8]
	want := []byte{0, 0, 0, 0, 3, 0, 0, 0}
	if !bytes.Equal(head, want) {
		t.Errorf("kop = %v, wil %v", head, want)
	}
	if got := buf.Len(); got != 32 {
		t.Errorf("bericht is %d bytes, wil 32", got)
	}
}
