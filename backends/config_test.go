package backends

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

func TestLoadConfig_EmptyPath(t *testing.T) {
	_, err := LoadConfig("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty path")
}

func TestLoadConfig_WhitespacePath(t *testing.T) {
	_, err := LoadConfig("   ")
	assert.Error(t, err)
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	assert.Error(t, err)
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	path := writeTemp(t, ":::not valid yaml:::")
	_, err := LoadConfig(path)
	assert.Error(t, err)
}

func TestLoadConfig_ValidETHOnly(t *testing.T) {
	path := writeTemp(t, `
private_key_path: ./key.hex
coordinators:
  - type: eth
    eth:
      ledger_id: 1337
      chain_url: "ws://127.0.0.1:8545"
      adjudicator_addr: "0xABCD"
  - type: eth
    eth:
      ledger_id: 1338
      chain_url: "ws://127.0.0.1:8546"
      adjudicator_addr: "0xEF01"
`)
	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "./key.hex", cfg.PrivateKeyPath)
	require.Len(t, cfg.Coordinators, 2)
	assert.Equal(t, "eth", cfg.Coordinators[0].Type)
	require.NotNil(t, cfg.Coordinators[0].ETH)
	assert.Equal(t, uint64(1337), cfg.Coordinators[0].ETH.LedgerID)
	assert.Equal(t, "ws://127.0.0.1:8545", cfg.Coordinators[0].ETH.ChainURL)
	assert.Equal(t, "0xABCD", cfg.Coordinators[0].ETH.AdjudicatorAddr)
}

func TestLoadConfig_ValidMixedETHAndCKB(t *testing.T) {
	path := writeTemp(t, `
private_key_path: ./key.hex
coordinators:
  - type: eth
    eth:
      ledger_id: 1337
      chain_url: "ws://127.0.0.1:8545"
      adjudicator_addr: "0xABCD"
  - type: ckb
    ckb:
      rpc_url: "http://127.0.0.1:8114"
      deployment_file: "./ckb_deployment.json"
      network: "devnet"
      signer_address: "ckt1qyqd0rfh3..."
      use_evm_signer: true
`)
	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, cfg.Coordinators, 2)
	assert.Equal(t, "ckb", cfg.Coordinators[1].Type)
	require.NotNil(t, cfg.Coordinators[1].CKB)
	assert.Equal(t, "http://127.0.0.1:8114", cfg.Coordinators[1].CKB.RPCURL)
	assert.Equal(t, "./ckb_deployment.json", cfg.Coordinators[1].CKB.DeploymentFile)
	assert.Equal(t, "devnet", cfg.Coordinators[1].CKB.Network)
	assert.True(t, cfg.Coordinators[1].CKB.UseEVMSigner)
}

// validETH returns a fully-populated eth entry so tests can override one field.
func validETH(ledgerID uint64) BackendCoordinatorConfig {
	return BackendCoordinatorConfig{
		Type: "eth",
		ETH: &ETHBackendConfig{
			LedgerID:        ledgerID,
			ChainURL:        "ws://127.0.0.1:8545",
			AdjudicatorAddr: "0xABCD",
		},
	}
}

// validCKB returns a fully-populated ckb entry so tests can override one field.
func validCKB() BackendCoordinatorConfig {
	return BackendCoordinatorConfig{
		Type: "ckb",
		CKB: &CKBBackendConfig{
			RPCURL:         "http://127.0.0.1:8114",
			DeploymentFile: "./ckb_deployment.json",
			Network:        "devnet",
			SignerAddress:  "ckt1qyqd0rfh3...",
		},
	}
}

func TestValidate_MissingPrivateKeyPath(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "",
		Coordinators:   []BackendCoordinatorConfig{validETH(1337)},
	}
	assert.Error(t, cfg.Validate())
}

func TestValidate_EmptyCoordinators(t *testing.T) {
	cfg := Config{PrivateKeyPath: "./key.hex"}
	assert.Error(t, cfg.Validate())
}

func TestValidate_MissingType(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators: []BackendCoordinatorConfig{
			{ETH: &ETHBackendConfig{LedgerID: 1337, ChainURL: "ws://x", AdjudicatorAddr: "0x1"}},
		},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "`type` is required")
}

func TestValidate_UnknownType(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators:   []BackendCoordinatorConfig{{Type: "solana"}},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown type")
}

func TestValidate_ETHTypeWithoutBlock(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators:   []BackendCoordinatorConfig{{Type: "eth"}},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires an `eth:` block")
}

func TestValidate_ETHTypeWithCKBBlock(t *testing.T) {
	c := validETH(1337)
	c.CKB = &CKBBackendConfig{}
	cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not set a `ckb:` block")
}

func TestValidate_CKBTypeWithoutBlock(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators:   []BackendCoordinatorConfig{{Type: "ckb"}},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a `ckb:` block")
}

func TestValidate_DuplicateETHLedger(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators:   []BackendCoordinatorConfig{validETH(1337), validETH(1337)},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}

func TestValidate_DuplicateCKB(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators:   []BackendCoordinatorConfig{validCKB(), validCKB()},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}

func TestValidate_ETHandCKBCoexist(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators:   []BackendCoordinatorConfig{validETH(1337), validCKB()},
	}
	assert.NoError(t, cfg.Validate())
}

func TestValidate_TwoETHChainsCoexist(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators:   []BackendCoordinatorConfig{validETH(1337), validETH(1338)},
	}
	assert.NoError(t, cfg.Validate())
}

func TestValidate_ETH_HTTPChainURLRejected(t *testing.T) {
	c := validETH(1337)
	c.ETH.ChainURL = "http://127.0.0.1:8545"
	cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ws://")
}

func TestValidate_ETH_EmptyChainURLRejected(t *testing.T) {
	c := validETH(1337)
	c.ETH.ChainURL = ""
	cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chain_url is required")
}

func TestValidate_ETH_WSSAccepted(t *testing.T) {
	c := validETH(1337)
	c.ETH.ChainURL = "wss://example.com:443"
	cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
	assert.NoError(t, cfg.Validate())
}

func TestValidate_ETH_EmptyAdjudicatorRejected(t *testing.T) {
	c := validETH(1337)
	c.ETH.AdjudicatorAddr = ""
	cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "adjudicator_addr is required")
}

func TestValidate_CKB_WSRejected(t *testing.T) {
	c := validCKB()
	c.CKB.RPCURL = "ws://127.0.0.1:8114"
	cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http://")
}

func TestValidate_CKB_EmptyRPCRejected(t *testing.T) {
	c := validCKB()
	c.CKB.RPCURL = ""
	cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rpc_url is required")
}

func TestValidate_CKB_MissingDeploymentFile(t *testing.T) {
	c := validCKB()
	c.CKB.DeploymentFile = ""
	cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deployment_file is required")
}

func TestValidate_CKB_MissingSignerAddress(t *testing.T) {
	c := validCKB()
	c.CKB.SignerAddress = ""
	cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signer_address is required")
}

func TestValidate_CKB_MissingNetwork(t *testing.T) {
	c := validCKB()
	c.CKB.Network = ""
	cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network is required")
}

func TestValidate_CKB_UnknownNetwork(t *testing.T) {
	c := validCKB()
	c.CKB.Network = "ropsten"
	cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown network")
}

func TestValidate_CKB_AllNetworksAccepted(t *testing.T) {
	for _, net := range []string{"mainnet", "testnet", "devnet"} {
		t.Run(net, func(t *testing.T) {
			c := validCKB()
			c.CKB.Network = net
			cfg := Config{PrivateKeyPath: "./key.hex", Coordinators: []BackendCoordinatorConfig{c}}
			assert.NoError(t, cfg.Validate())
		})
	}
}

func TestBackendIDForType(t *testing.T) {
	id, ok := BackendIDForType("eth")
	require.True(t, ok)
	assert.Equal(t, uint32(1), id)

	id, ok = BackendIDForType("ckb")
	require.True(t, ok)
	assert.Equal(t, uint32(3), id)

	_, ok = BackendIDForType("solana")
	assert.False(t, ok)
}
