package backends

import (
	"fmt"
	"os"
	"strings"

	"github.com/stretchr/testify/assert/yaml"
)

type Config struct {
	PrivateKeyPath string                     `yaml:"private_key_path"`
	Coordinators   []BackendCoordinatorConfig `yaml:"coordinators"`
}

// BackendCoordinatorConfig is a tagged union: Type selects exactly one of the
// embedded typed configs (ETH or CKB). The discriminator lets us validate and
// dispatch in SetupMultiCoordinator without adding backend-specific fields to
// a shared shape.
type BackendCoordinatorConfig struct {
	Type string            `yaml:"type"` // "eth" | "ckb"
	ETH  *ETHBackendConfig `yaml:"eth,omitempty"`
	CKB  *CKBBackendConfig `yaml:"ckb,omitempty"`
}

// ETHBackendConfig configures one Ethereum coordinator instance.
type ETHBackendConfig struct {
	LedgerID        uint64 `yaml:"ledger_id"`        // chain ID
	ChainURL        string `yaml:"chain_url"`        // ws:// or wss:// (SubscribeNewHead requires it)
	AdjudicatorAddr string `yaml:"adjudicator_addr"` // deployed Adjudicator address
}

// CKBBackendConfig configures one Nervos CKB coordinator instance. CKB always
// uses the fixed asset.CKBBackendID == 3 with ContractLID "03" — there is no
// per-chain LedgerID variability the way ETH has chain IDs.
type CKBBackendConfig struct {
	RPCURL         string `yaml:"rpc_url"`         // CKB JSON-RPC endpoint (http:// or https://)
	DeploymentFile string `yaml:"deployment_file"` // path to JSON-serialised perun-ckb-backend.Deployment
	Network        string `yaml:"network"`         // "mainnet" | "testnet" | "devnet"
	SignerAddress  string `yaml:"signer_address"`  // operator's CKB address (ckb1... / ckt1...)
	UseEVMSigner   bool   `yaml:"use_evm_signer"`  // false → secp256k1_blake160 sighash; true → omni-lock EVM-auth
}

// BackendIDForType maps the YAML discriminator to the wallet.BackendID the
// rest of go-perun uses. Keep in sync with perun-eth-backend's EthBackendID
// (1) and perun-ckb-backend's CKBBackendID (3).
func BackendIDForType(t string) (uint32, bool) {
	switch t {
	case "eth":
		return 1, true
	case "ckb":
		return 3, true
	default:
		return 0, false
	}
}

func LoadConfig(path string) (Config, error) {

	if strings.TrimSpace(path) == "" {
		return Config{}, fmt.Errorf("load config: empty path")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("load config: read %s: %w", path, err)
	}

	cfg := Config{
		PrivateKeyPath: "",
		Coordinators:   []BackendCoordinatorConfig{},
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("load config: unmarshal %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("load config: validate %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks required config fields and key uniqueness constraints.
func (c Config) Validate() error {
	if strings.TrimSpace(c.PrivateKeyPath) == "" {
		return fmt.Errorf("load config: private_key_path is required")
	}

	if len(c.Coordinators) == 0 {
		return fmt.Errorf("load config: coordinators is required")
	}
	seen := make(map[string]struct{}, len(c.Coordinators))
	for i, coord := range c.Coordinators {
		if err := coord.validate(i); err != nil {
			return err
		}

		key := coord.uniqueKey()
		if _, ok := seen[key]; ok {
			return fmt.Errorf("load config: duplicate coordinator key %q", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// validate enforces the per-entry discriminator + per-backend rules.
func (e BackendCoordinatorConfig) validate(i int) error {
	switch e.Type {
	case "eth":
		if e.ETH == nil {
			return fmt.Errorf("load config: coordinators[%d]: type=eth requires an `eth:` block", i)
		}
		if e.CKB != nil {
			return fmt.Errorf("load config: coordinators[%d]: type=eth must not set a `ckb:` block", i)
		}
		return e.ETH.validate(i)
	case "ckb":
		if e.CKB == nil {
			return fmt.Errorf("load config: coordinators[%d]: type=ckb requires a `ckb:` block", i)
		}
		if e.ETH != nil {
			return fmt.Errorf("load config: coordinators[%d]: type=ckb must not set an `eth:` block", i)
		}
		return e.CKB.validate(i)
	case "":
		return fmt.Errorf("load config: coordinators[%d]: `type` is required (eth|ckb)", i)
	default:
		return fmt.Errorf("load config: coordinators[%d]: unknown type %q (want eth|ckb)", i, e.Type)
	}
}

// uniqueKey produces the key used for duplicate detection across coordinator
// entries. ETH chains are unique per (backend, ledger). CKB has a fixed
// ledger ID so at most one entry is permitted.
func (e BackendCoordinatorConfig) uniqueKey() string {
	switch e.Type {
	case "eth":
		return fmt.Sprintf("eth/%d", e.ETH.LedgerID)
	case "ckb":
		return "ckb"
	default:
		return e.Type
	}
}

func (c ETHBackendConfig) validate(i int) error {
	url := strings.TrimSpace(c.ChainURL)
	if url == "" {
		return fmt.Errorf("load config: coordinators[%d].eth: chain_url is required", i)
	}
	// chain_url must use a WebSocket transport. SubscribeNewHead (used by
	// BlockTimeout.Wait and confirmNTimes in the ETH backend) only works over
	// ws:// or wss://; HTTP transports silently fail at first use.
	if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
		return fmt.Errorf("load config: coordinators[%d].eth: chain_url must use ws:// or wss:// (got %q)", i, url)
	}
	if strings.TrimSpace(c.AdjudicatorAddr) == "" {
		return fmt.Errorf("load config: coordinators[%d].eth: adjudicator_addr is required", i)
	}
	return nil
}

func (c CKBBackendConfig) validate(i int) error {
	url := strings.TrimSpace(c.RPCURL)
	if url == "" {
		return fmt.Errorf("load config: coordinators[%d].ckb: rpc_url is required", i)
	}
	// CKB JSON-RPC is HTTP-only (subscriptions use polling, not ws upgrades).
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("load config: coordinators[%d].ckb: rpc_url must use http:// or https:// (got %q)", i, url)
	}
	if strings.TrimSpace(c.DeploymentFile) == "" {
		return fmt.Errorf("load config: coordinators[%d].ckb: deployment_file is required", i)
	}
	if strings.TrimSpace(c.SignerAddress) == "" {
		return fmt.Errorf("load config: coordinators[%d].ckb: signer_address is required", i)
	}
	switch c.Network {
	case "mainnet", "testnet", "devnet":
	case "":
		return fmt.Errorf("load config: coordinators[%d].ckb: network is required (mainnet|testnet|devnet)", i)
	default:
		return fmt.Errorf("load config: coordinators[%d].ckb: unknown network %q", i, c.Network)
	}
	return nil
}
