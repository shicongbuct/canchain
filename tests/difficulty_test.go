package tests

import (
	"math/big"
	"testing"

	"github.com/5uwifi/canchain/common"
	"github.com/5uwifi/canchain/params"
)

var (
	mainnetChainConfig = params.ChainConfig{
		ChainID:        big.NewInt(1),
		HomesteadBlock: big.NewInt(1150000),
		DAOForkBlock:   big.NewInt(1920000),
		DAOForkSupport: true,
		EIP150Block:    big.NewInt(2463000),
		EIP150Hash:     common.HexToHash("0x2086799aeebeae135c246c65021c82b4e15a2c451340993aacfd2751886514f0"),
		EIP155Block:    big.NewInt(2675000),
		EIP158Block:    big.NewInt(2675000),
		ByzantiumBlock: big.NewInt(4370000),
	}

	RopstenNoConstantinople = params.ChainConfig{
		ChainID:             big.NewInt(3),
		HomesteadBlock:      big.NewInt(0),
		DAOForkBlock:        nil,
		DAOForkSupport:      true,
		EIP150Block:         big.NewInt(0),
		EIP150Hash:          common.HexToHash("0x41941023680923e0fe4d74a34bdac8141f2540e3ae90623718e47d66d1ca4a2d"),
		EIP155Block:         big.NewInt(10),
		EIP158Block:         big.NewInt(10),
		ByzantiumBlock:      big.NewInt(1700000),
		ConstantinopleBlock: nil,
	}
)

func TestDifficulty(t *testing.T) {
	t.Parallel()

	dt := new(testMatcher)
	dt.skipLoad("hexencodetest.*")
	dt.skipLoad("crypto.*")
	dt.skipLoad("blockgenesistest\\.json")
	dt.skipLoad("genesishashestest\\.json")
	dt.skipLoad("keyaddrtest\\.json")
	dt.skipLoad("txtest\\.json")

	dt.skipLoad("difficultyCustomHomestead\\.json")
	dt.skipLoad("difficultyMorden\\.json")
	dt.skipLoad("difficultyOlimpic\\.json")

	dt.config("Ropsten", RopstenNoConstantinople)
	dt.config("Morden", *params.TestnetChainConfig)
	dt.config("Frontier", params.ChainConfig{})

	dt.config("Homestead", params.ChainConfig{
		HomesteadBlock: big.NewInt(0),
	})

	dt.config("Byzantium", params.ChainConfig{
		ByzantiumBlock: big.NewInt(0),
	})

	dt.config("Frontier", *params.TestnetChainConfig)
	dt.config("MainNetwork", mainnetChainConfig)
	dt.config("CustomMainNetwork", mainnetChainConfig)
	dt.config("difficulty.json", mainnetChainConfig)

	dt.walk(t, difficultyTestDir, func(t *testing.T, name string, test *DifficultyTest) {
		cfg := dt.findConfig(name)
		if test.ParentDifficulty.Cmp(params.MinimumDifficulty) < 0 {
			t.Skip("difficulty below minimum")
			return
		}
		if err := dt.checkFailure(t, name, test.Run(cfg)); err != nil {
			t.Error(err)
		}
	})
}
