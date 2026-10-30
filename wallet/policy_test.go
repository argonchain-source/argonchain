package wallet

import (
	"testing"

	"argonmine/mempool"
)

// The wallet pays the nodes' relay surcharge for asset outputs; the two
// must agree, or a split or a multi-recipient asset payment is refused.
func TestAssetSurchargeMatchesMempool(t *testing.T) {
	cfg := mempool.DefaultConfig()
	if assetOutputFee != cfg.AssetOutputFee || freeAssetOutputs != mempool.FreeAssetOutputs || DustLimit != cfg.DustLimit {
		t.Fatalf("wallet policy (%d per asset output beyond %d, dust %d) differs from the nodes' (%d beyond %d, dust %d)",
			assetOutputFee, freeAssetOutputs, DustLimit, cfg.AssetOutputFee, mempool.FreeAssetOutputs, cfg.DustLimit)
	}
}
