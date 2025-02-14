package txmgr

import (
	"bytes"
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"

	feetypes "github.com/smartcontractkit/chainlink/v2/common/fee/types"
	txmgrtypes "github.com/smartcontractkit/chainlink/v2/common/txmgr/types"
	commontypes "github.com/smartcontractkit/chainlink/v2/common/types"
	"github.com/smartcontractkit/chainlink/v2/core/chains/evm/assets"
	"github.com/smartcontractkit/chainlink/v2/core/chains/evm/gas"
	evmtypes "github.com/smartcontractkit/chainlink/v2/core/chains/evm/types"
	"github.com/smartcontractkit/chainlink/v2/core/logger"
)

type TxAttemptSigner[ADDR commontypes.Hashable] interface {
	SignTx(fromAddress ADDR, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)
}

var _ TxAttemptBuilder = (*evmTxAttemptBuilder)(nil)

type evmTxAttemptBuilder struct {
	chainID   big.Int
	feeConfig evmTxAttemptBuilderFeeConfig
	keystore  TxAttemptSigner[common.Address]
	gas.EvmFeeEstimator
}

type evmTxAttemptBuilderFeeConfig interface {
	EIP1559DynamicFees() bool
	TipCapMin() *assets.Wei
	PriceMin() *assets.Wei
	PriceMaxKey(common.Address) *assets.Wei
}

func NewEvmTxAttemptBuilder(chainID big.Int, feeConfig evmTxAttemptBuilderFeeConfig, keystore TxAttemptSigner[common.Address], estimator gas.EvmFeeEstimator) *evmTxAttemptBuilder {
	return &evmTxAttemptBuilder{chainID, feeConfig, keystore, estimator}
}

// NewTxAttempt builds an new attempt using the configured fee estimator + using the EIP1559 config to determine tx type
// used for when a brand new transaction is being created in the txm
func (c *evmTxAttemptBuilder) NewTxAttempt(ctx context.Context, etx Tx, lggr logger.Logger, opts ...feetypes.Opt) (attempt TxAttempt, fee gas.EvmFee, feeLimit uint32, retryable bool, err error) {
	txType := 0x0
	if c.feeConfig.EIP1559DynamicFees() {
		txType = 0x2
	}
	return c.NewTxAttemptWithType(ctx, etx, lggr, txType, opts...)
}

// NewTxAttemptWithType builds a new attempt with a new fee estimation where the txType can be specified by the caller
// used for L2 re-estimation on broadcasting (note EIP1559 must be disabled otherwise this will fail with mismatched fees + tx type)
func (c *evmTxAttemptBuilder) NewTxAttemptWithType(ctx context.Context, etx Tx, lggr logger.Logger, txType int, opts ...feetypes.Opt) (attempt TxAttempt, fee gas.EvmFee, feeLimit uint32, retryable bool, err error) {
	keySpecificMaxGasPriceWei := c.feeConfig.PriceMaxKey(etx.FromAddress)
	fee, feeLimit, err = c.EvmFeeEstimator.GetFee(ctx, etx.EncodedPayload, etx.FeeLimit, keySpecificMaxGasPriceWei, opts...)
	if err != nil {
		return attempt, fee, feeLimit, true, errors.Wrap(err, "failed to get fee") // estimator errors are retryable
	}

	attempt, retryable, err = c.NewCustomTxAttempt(etx, fee, feeLimit, txType, lggr)
	return attempt, fee, feeLimit, retryable, err
}

// NewBumpTxAttempt builds a new attempt with a bumped fee - based on the previous attempt tx type
// used in the txm broadcaster + confirmer when tx ix rejected for too low fee or is not included in a timely manner
func (c *evmTxAttemptBuilder) NewBumpTxAttempt(ctx context.Context, etx Tx, previousAttempt TxAttempt, priorAttempts []TxAttempt, lggr logger.Logger) (attempt TxAttempt, bumpedFee gas.EvmFee, bumpedFeeLimit uint32, retryable bool, err error) {
	keySpecificMaxGasPriceWei := c.feeConfig.PriceMaxKey(etx.FromAddress)

	bumpedFee, bumpedFeeLimit, err = c.EvmFeeEstimator.BumpFee(ctx, previousAttempt.TxFee, etx.FeeLimit, keySpecificMaxGasPriceWei, newEvmPriorAttempts(priorAttempts))
	if err != nil {
		return attempt, bumpedFee, bumpedFeeLimit, true, errors.Wrap(err, "failed to bump fee") // estimator errors are retryable
	}

	attempt, retryable, err = c.NewCustomTxAttempt(etx, bumpedFee, bumpedFeeLimit, previousAttempt.TxType, lggr)
	return attempt, bumpedFee, bumpedFeeLimit, retryable, err
}

// NewCustomTxAttempt is the lowest level func where the fee parameters + tx type must be passed in
// used in the txm for force rebroadcast where fees and tx type are pre-determined without an estimator
func (c *evmTxAttemptBuilder) NewCustomTxAttempt(etx Tx, fee gas.EvmFee, gasLimit uint32, txType int, lggr logger.Logger) (attempt TxAttempt, retryable bool, err error) {
	switch txType {
	case 0x0: // legacy
		if fee.Legacy == nil {
			err = errors.Errorf("Attempt %v is a type 0 transaction but estimator did not return legacy fee bump", attempt.ID)
			logger.Sugared(lggr).AssumptionViolation(err.Error())
			return attempt, false, err // not retryable
		}
		attempt, err = c.newLegacyAttempt(etx, fee.Legacy, gasLimit)
		return attempt, true, err
	case 0x2: // dynamic, EIP1559
		if !fee.ValidDynamic() {
			err = errors.Errorf("Attempt %v is a type 2 transaction but estimator did not return dynamic fee bump", attempt.ID)
			logger.Sugared(lggr).AssumptionViolation(err.Error())
			return attempt, false, err // not retryable
		}
		attempt, err = c.newDynamicFeeAttempt(etx, gas.DynamicFee{
			FeeCap: fee.DynamicFeeCap,
			TipCap: fee.DynamicTipCap,
		}, gasLimit)
		return attempt, true, err
	default:
		err = errors.Errorf("invariant violation: Attempt %v had unrecognised transaction type %v"+
			"This is a bug! Please report to https://github.com/smartcontractkit/chainlink/issues", attempt.ID, attempt.TxType)
		logger.Sugared(lggr).AssumptionViolation(err.Error())
		return attempt, false, err // not retryable
	}
}

// NewEmptyTxAttempt is used in ForceRebroadcast to create a signed tx with zero value sent to the zero address
func (c *evmTxAttemptBuilder) NewEmptyTxAttempt(nonce evmtypes.Nonce, feeLimit uint32, fee gas.EvmFee, fromAddress common.Address) (attempt TxAttempt, err error) {
	value := big.NewInt(0)
	payload := []byte{}

	if fee.Legacy == nil {
		return attempt, errors.New("NewEmptyTranscation: legacy fee cannot be nil")
	}

	tx := types.NewTransaction(uint64(nonce), fromAddress, value, uint64(feeLimit), fee.Legacy.ToInt(), payload)

	hash, signedTxBytes, err := c.SignTx(fromAddress, tx)
	if err != nil {
		return attempt, errors.Wrapf(err, "error using account %s to sign empty transaction", fromAddress.String())
	}

	attempt.SignedRawTx = signedTxBytes
	attempt.Hash = hash
	return attempt, nil

}

func (c *evmTxAttemptBuilder) newDynamicFeeAttempt(etx Tx, fee gas.DynamicFee, gasLimit uint32) (attempt TxAttempt, err error) {
	if err = validateDynamicFeeGas(c.feeConfig, c.feeConfig.TipCapMin(), fee, gasLimit, etx); err != nil {
		return attempt, errors.Wrap(err, "error validating gas")
	}

	d := newDynamicFeeTransaction(
		uint64(*etx.Sequence),
		etx.ToAddress,
		&etx.Value,
		gasLimit,
		&c.chainID,
		fee.TipCap,
		fee.FeeCap,
		etx.EncodedPayload,
	)
	tx := types.NewTx(&d)
	attempt, err = c.newSignedAttempt(etx, tx)
	if err != nil {
		return attempt, err
	}
	attempt.TxFee = gas.EvmFee{
		DynamicFeeCap: fee.FeeCap,
		DynamicTipCap: fee.TipCap,
	}
	attempt.ChainSpecificFeeLimit = gasLimit
	attempt.TxType = 2
	return attempt, nil
}

var Max256BitUInt = big.NewInt(0).Exp(big.NewInt(2), big.NewInt(256), nil)

type keySpecificEstimator interface {
	PriceMaxKey(addr common.Address) *assets.Wei
}

// validateDynamicFeeGas is a sanity check - we have other checks elsewhere, but this
// makes sure we _never_ create an invalid attempt
func validateDynamicFeeGas(kse keySpecificEstimator, tipCapMinimum *assets.Wei, fee gas.DynamicFee, gasLimit uint32, etx Tx) error {
	gasTipCap, gasFeeCap := fee.TipCap, fee.FeeCap

	if gasTipCap == nil {
		panic("gas tip cap missing")
	}
	if gasFeeCap == nil {
		panic("gas fee cap missing")
	}
	// Assertions from:	https://github.com/ethereum/EIPs/blob/master/EIPS/eip-1559.md
	// Prevent impossibly large numbers
	if gasFeeCap.ToInt().Cmp(Max256BitUInt) > 0 {
		return errors.New("impossibly large fee cap")
	}
	if gasTipCap.ToInt().Cmp(Max256BitUInt) > 0 {
		return errors.New("impossibly large tip cap")
	}
	// The total must be at least as large as the tip
	if gasFeeCap.Cmp(gasTipCap) < 0 {
		return errors.Errorf("gas fee cap must be greater than or equal to gas tip cap (fee cap: %s, tip cap: %s)", gasFeeCap.String(), gasTipCap.String())
	}

	// Configuration sanity-check
	max := kse.PriceMaxKey(etx.FromAddress)
	if gasFeeCap.Cmp(max) > 0 {
		return errors.Errorf("cannot create tx attempt: specified gas fee cap of %s would exceed max configured gas price of %s for key %s", gasFeeCap.String(), max.String(), etx.FromAddress.String())
	}
	// Tip must be above minimum
	minTip := tipCapMinimum
	if gasTipCap.Cmp(minTip) < 0 {
		return errors.Errorf("cannot create tx attempt: specified gas tip cap of %s is below min configured gas tip of %s for key %s", gasTipCap.String(), minTip.String(), etx.FromAddress.String())
	}
	return nil
}

func newDynamicFeeTransaction(nonce uint64, to common.Address, value *big.Int, gasLimit uint32, chainID *big.Int, gasTipCap, gasFeeCap *assets.Wei, data []byte) types.DynamicFeeTx {
	return types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: gasTipCap.ToInt(),
		GasFeeCap: gasFeeCap.ToInt(),
		Gas:       uint64(gasLimit),
		To:        &to,
		Value:     value,
		Data:      data,
	}
}

func (c *evmTxAttemptBuilder) newLegacyAttempt(etx Tx, gasPrice *assets.Wei, gasLimit uint32) (attempt TxAttempt, err error) {
	if err = validateLegacyGas(c.feeConfig, c.feeConfig.PriceMin(), gasPrice, gasLimit, etx); err != nil {
		return attempt, errors.Wrap(err, "error validating gas")
	}

	tx := newLegacyTransaction(
		uint64(*etx.Sequence),
		etx.ToAddress,
		&etx.Value,
		gasLimit,
		gasPrice,
		etx.EncodedPayload,
	)

	transaction := types.NewTx(&tx)
	hash, signedTxBytes, err := c.SignTx(etx.FromAddress, transaction)
	if err != nil {
		return attempt, errors.Wrapf(err, "error using account %s to sign transaction %v", etx.FromAddress, etx.ID)
	}

	attempt.State = txmgrtypes.TxAttemptInProgress
	attempt.SignedRawTx = signedTxBytes
	attempt.TxID = etx.ID
	attempt.TxFee = gas.EvmFee{Legacy: gasPrice}
	attempt.Hash = hash
	attempt.TxType = 0
	attempt.ChainSpecificFeeLimit = gasLimit
	attempt.Tx = etx

	return attempt, nil
}

// validateLegacyGas is a sanity check - we have other checks elsewhere, but this
// makes sure we _never_ create an invalid attempt
func validateLegacyGas(kse keySpecificEstimator, minGasPriceWei, gasPrice *assets.Wei, gasLimit uint32, etx Tx) error {
	if gasPrice == nil {
		panic("gas price missing")
	}
	max := kse.PriceMaxKey(etx.FromAddress)
	if gasPrice.Cmp(max) > 0 {
		return errors.Errorf("cannot create tx attempt: specified gas price of %s would exceed max configured gas price of %s for key %s", gasPrice.String(), max.String(), etx.FromAddress.String())
	}
	min := minGasPriceWei
	if gasPrice.Cmp(min) < 0 {
		return errors.Errorf("cannot create tx attempt: specified gas price of %s is below min configured gas price of %s for key %s", gasPrice.String(), min.String(), etx.FromAddress.String())
	}
	return nil
}

func (c *evmTxAttemptBuilder) newSignedAttempt(etx Tx, tx *types.Transaction) (attempt TxAttempt, err error) {
	hash, signedTxBytes, err := c.SignTx(etx.FromAddress, tx)
	if err != nil {
		return attempt, errors.Wrapf(err, "error using account %s to sign transaction %v", etx.FromAddress.String(), etx.ID)
	}

	attempt.State = txmgrtypes.TxAttemptInProgress
	attempt.SignedRawTx = signedTxBytes
	attempt.TxID = etx.ID
	attempt.Tx = etx
	attempt.Hash = hash

	return attempt, nil
}

func newLegacyTransaction(nonce uint64, to common.Address, value *big.Int, gasLimit uint32, gasPrice *assets.Wei, data []byte) types.LegacyTx {
	return types.LegacyTx{
		Nonce:    nonce,
		To:       &to,
		Value:    value,
		Gas:      uint64(gasLimit),
		GasPrice: gasPrice.ToInt(),
		Data:     data,
	}
}

func (c *evmTxAttemptBuilder) SignTx(address common.Address, tx *types.Transaction) (common.Hash, []byte, error) {
	signedTx, err := c.keystore.SignTx(address, tx, &c.chainID)
	if err != nil {
		return common.Hash{}, nil, errors.Wrap(err, "SignTx failed")
	}
	rlp := new(bytes.Buffer)
	if err := signedTx.EncodeRLP(rlp); err != nil {
		return common.Hash{}, nil, errors.Wrap(err, "SignTx failed")
	}
	txHash := signedTx.Hash()
	return txHash, rlp.Bytes(), nil
}

func newEvmPriorAttempts(attempts []TxAttempt) (prior []gas.EvmPriorAttempt) {
	for i := range attempts {
		priorAttempt := gas.EvmPriorAttempt{
			ChainSpecificFeeLimit:   attempts[i].ChainSpecificFeeLimit,
			BroadcastBeforeBlockNum: attempts[i].BroadcastBeforeBlockNum,
			TxHash:                  attempts[i].Hash,
			TxType:                  attempts[i].TxType,
			GasPrice:                attempts[i].TxFee.Legacy,
			DynamicFee: gas.DynamicFee{
				FeeCap: attempts[i].TxFee.DynamicFeeCap,
				TipCap: attempts[i].TxFee.DynamicTipCap,
			},
		}
		prior = append(prior, priorAttempt)
	}
	return
}
