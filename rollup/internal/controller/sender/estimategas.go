package sender

import (
	"fmt"
	"math/big"

	"github.com/scroll-tech/go-ethereum"
	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/core/types"
	"github.com/scroll-tech/go-ethereum/log"
)

func (s *Sender) estimateLegacyGas(to *common.Address, value *big.Int, data []byte, fallbackGasLimit uint64) (*FeeData, error) {
	gasPrice, err := s.client.SuggestGasPrice(s.ctx)
	if err != nil {
		log.Error("estimateLegacyGas SuggestGasPrice failure", "error", err)
		return nil, err
	}
	gasLimit, _, err := s.estimateGasLimit(to, data, gasPrice, nil, nil, value, false)
	if err != nil {
		log.Error("estimateLegacyGas estimateGasLimit failure", "gas price", gasPrice, "from", s.auth.From.String(),
			"nonce", s.auth.Nonce.Uint64(), "to address", to.String(), "fallback gas limit", fallbackGasLimit, "error", err)
		if fallbackGasLimit == 0 {
			return nil, err
		}
		gasLimit = fallbackGasLimit
	} else {
		gasLimit = gasLimit * 12 / 10 // 20% extra gas to avoid out of gas error
	}
	return &FeeData{
		gasPrice: gasPrice,
		gasLimit: gasLimit,
	}, nil
}

func (s *Sender) estimateDynamicGas(to *common.Address, value *big.Int, data []byte, fallbackGasLimit uint64, baseFee uint64) (*FeeData, error) {
	gasTipCap, err := s.client.SuggestGasTipCap(s.ctx)
	if err != nil {
		log.Error("estimateDynamicGas SuggestGasTipCap failure", "error", err)
		return nil, err
	}

	gasFeeCap := new(big.Int).Add(gasTipCap, new(big.Int).Mul(new(big.Int).SetUint64(baseFee), big.NewInt(2)))
	gasLimit, accessList, err := s.estimateGasLimit(to, data, nil, gasTipCap, gasFeeCap, value, true)
	if err != nil {
		log.Error("estimateDynamicGas estimateGasLimit failure",
			"from", s.auth.From.String(), "nonce", s.auth.Nonce.Uint64(), "to address", to.String(),
			"fallback gas limit", fallbackGasLimit, "error", err)
		if fallbackGasLimit == 0 {
			return nil, err
		}
		gasLimit = fallbackGasLimit
	} else {
		gasLimit = gasLimit * 12 / 10 // 20% extra gas to avoid out of gas error
	}
	feeData := &FeeData{
		gasLimit:  gasLimit,
		gasTipCap: gasTipCap,
		gasFeeCap: gasFeeCap,
	}
	if accessList != nil {
		feeData.accessList = *accessList
	}
	return feeData, nil
}

func (s *Sender) estimateGasLimit(to *common.Address, data []byte, gasPrice, gasTipCap, gasFeeCap, value *big.Int, useAccessList bool) (uint64, *types.AccessList, error) {
	msg := ethereum.CallMsg{
		From:      s.auth.From,
		To:        to,
		GasPrice:  gasPrice,
		GasTipCap: gasTipCap,
		GasFeeCap: gasFeeCap,
		Value:     value,
		Data:      data,
	}
	gasLimitWithoutAccessList, err := s.client.EstimateGas(s.ctx, msg)
	if err != nil {
		log.Error("estimateGasLimit EstimateGas failure without access list", "error", err)
		return 0, nil, err
	}

	if !useAccessList {
		return gasLimitWithoutAccessList, nil, nil
	}

	// Explicitly set a gas limit to prevent the "insufficient funds for gas * price + value" error.
	// Because if msg.Gas remains unset, CreateAccessList defaults to using RPCGasCap(), which can be excessively high.
	msg.Gas = gasLimitWithoutAccessList * 3
	accessList, gasLimitWithAccessList, errStr, rpcErr := s.gethClient.CreateAccessList(s.ctx, msg)
	if rpcErr != nil {
		log.Error("CreateAccessList RPC error", "error", rpcErr)
		return gasLimitWithoutAccessList, nil, rpcErr
	}
	if errStr != "" {
		log.Error("CreateAccessList reported error", "error", errStr)
		return gasLimitWithoutAccessList, nil, fmt.Errorf(errStr)
	}

	// Fine-tune accessList because 'to' address is automatically included in the access list by the Ethereum protocol: https://github.com/ethereum/go-ethereum/blob/v1.13.10/core/state/statedb.go#L1322
	// This function returns a gas estimation because GO SDK does not support access list: https://github.com/ethereum/go-ethereum/blob/v1.13.10/ethclient/ethclient.go#L642
	accessList, gasLimitWithAccessList = finetuneAccessList(accessList, gasLimitWithAccessList, msg.To)

	log.Info("gas", "senderName", s.name, "senderService", s.service, "gasLimitWithAccessList", gasLimitWithAccessList, "gasLimitWithoutAccessList", gasLimitWithoutAccessList, "accessList", accessList)

	if gasLimitWithAccessList < gasLimitWithoutAccessList {
		return gasLimitWithAccessList, accessList, nil
	}
	return gasLimitWithoutAccessList, nil, nil
}

func finetuneAccessList(accessList *types.AccessList, gasLimitWithAccessList uint64, to *common.Address) (*types.AccessList, uint64) {
	if accessList == nil || to == nil {
		return accessList, gasLimitWithAccessList
	}

	var newAccessList types.AccessList
	for _, entry := range *accessList {
		if entry.Address == *to && len(entry.StorageKeys) < 24 {
			// Based on: https://arxiv.org/pdf/2312.06574.pdf
			// We remove the address and respective storage keys as long as the number of storage keys < 24.
			// This removal helps in preventing double-counting of the 'to' address in access list calculations.
			gasLimitWithAccessList -= 2400
			// Each storage key saves 100 gas units.
			gasLimitWithAccessList += uint64(100 * len(entry.StorageKeys))
		} else {
			// Otherwise, keep the entry in the new access list.
			newAccessList = append(newAccessList, entry)
		}
	}
	return &newAccessList, gasLimitWithAccessList
}
