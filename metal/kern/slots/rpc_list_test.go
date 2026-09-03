package slots

import (
	"strings"
	"testing"

	"github.com/xinix00/HopOS/metal/v2/abi/hopabi"
)

func TestListRespWireLimit(t *testing.T) {
	req := hopabi.Req{Op: hopabi.OpList, Path: "/dir"}

	exact := decodeResp(t, listResp(req, []string{strings.Repeat("x", hopabi.MaxChunk)}))
	if exact.Status != hopabi.StatusOK || len(exact.Data) != hopabi.MaxChunk {
		t.Fatalf("exacte grens: status=%d bytes=%d", exact.Status, len(exact.Data))
	}

	tooLarge := decodeResp(t, listResp(req, []string{"x", strings.Repeat("y", hopabi.MaxChunk)}))
	if tooLarge.Status != hopabi.StatusError {
		t.Fatalf("te grote listing kreeg status %d, wil Error", tooLarge.Status)
	}
	if !strings.Contains(string(tooLarge.Data), "more than max") {
		t.Fatalf("onduidelijke grensfout: %q", tooLarge.Data)
	}
}
