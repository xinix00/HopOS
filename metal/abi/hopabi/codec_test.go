package hopabi

import (
	"reflect"
	"testing"
)

// De codec-ops dragen geen bytes maar AANWIJZINGEN, en daar mag geen veld in
// verschuiven: HOP leest ze op de rand van zijn vertrouwensgrens. Deze test
// zet elk veld op een andere waarde en eist ze allemaal terug.
func TestCodecRecordsOverlevenDeDraad(t *testing.T) {
	open := OpenArgs{Codec: 2, Dir: 1, Pixel: 4, Width: 3840, Height: 2160}
	if got, err := DecodeOpen(EncodeOpen(open)); err != nil || got != open {
		t.Errorf("open: %+v (%v), verwacht %+v", got, err, open)
	}
	feed := FeedArgs{Handle: 7, Flags: 3, Filled: 1 << 40, Tag: 0xCAFEBABEDEADBEEF}
	if got, err := DecodeFeed(EncodeFeed(feed)); err != nil || got != feed {
		t.Errorf("feed: %+v (%v), verwacht %+v", got, err, feed)
	}
	buf := BufArgs{Handle: 0xFFFFFFFF}
	if got, err := DecodeBuf(EncodeBuf(buf)); err != nil || got != buf {
		t.Errorf("buf: %+v (%v), verwacht %+v", got, err, buf)
	}

	ev := Event{
		Kind: EventProduced, Key: true, Pixel: 4,
		Width: 3840, Height: 2160,
		Off: 1 << 20, Size: 24883200, Bytes: 24883200, Tag: 99,
		Stride: [3]int{7680, 7680, 0},
		Plane:  [3]uint64{0, 16588800, 0},
	}
	b := make([]byte, EventLen)
	EncodeEvent(b, ev)
	got, err := DecodeEvent(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, ev) {
		t.Errorf("event kwam terug als %+v, verwacht %+v", got, ev)
	}

	// Een tweede event in dezelfde buffer mag niets van het eerste erven.
	EncodeEvent(b, Event{Kind: EventDone})
	got, _ = DecodeEvent(b)
	if (got != Event{Kind: EventDone}) {
		t.Errorf("hergebruikte buffer lekte velden: %+v", got)
	}
}

// Te korte records zijn een protocolfout, geen halve waarde.
func TestCodecRecordsWeigerenTeKorteInvoer(t *testing.T) {
	if _, err := DecodeOpen(make([]byte, OpenArgsLen-1)); err == nil {
		t.Error("open accepteerde een te kort record")
	}
	if _, err := DecodeFeed(make([]byte, FeedArgsLen-1)); err == nil {
		t.Error("feed accepteerde een te kort record")
	}
	if _, err := DecodeBuf(make([]byte, BufArgsLen-1)); err == nil {
		t.Error("buf accepteerde een te kort record")
	}
	if _, err := DecodeEvent(make([]byte, EventLen-1)); err == nil {
		t.Error("event accepteerde een te kort record")
	}
}
