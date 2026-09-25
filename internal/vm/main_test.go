package vm

import (
	"context"
	"os"
	"testing"
	"time"
)

// Tests never run nft: the bridge has no binding sets, so Boot binds nothing
// (tests of the bindings set their own fakes).
// realNftScript and realNftListSet are the real nft calls, for the
// integration test (FR_NFT_INTEGRATION=1, as root with nft).
var realNftScript, realNftListSet = nftScript, nftListSet

func TestMain(m *testing.M) {
	nftScript = func(context.Context, string) error { return ErrNoBindingSets }
	nftListSet = func(context.Context, string) ([]byte, error) { return nil, ErrNoBindingSets }
	linkExists = func(string) bool { return true }
	bindTries, bindPoll = 1, time.Millisecond
	os.Exit(m.Run())
}
