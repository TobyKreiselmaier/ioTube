// Copyright (c) 2020 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package relayer

import (
	"context"
	"crypto/ecdsa"
	"log"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/pkg/errors"

	"github.com/iotexproject/ioTube/witness-service/contract"
	"github.com/iotexproject/ioTube/witness-service/util"
	"github.com/iotexproject/iotex-address/address"
	"github.com/iotexproject/iotex-antenna-go/v2/iotex"
	"github.com/iotexproject/iotex-core/pkg/unit"
	"github.com/iotexproject/iotex-proto/golang/iotexapi"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
)

// transferValidatorOnIoTeX defines the transfer validator
type transferValidatorOnIoTeX struct {
	mu       sync.RWMutex
	gasLimit uint64
	gasPrice *big.Int

	privateKey            *ecdsa.PrivateKey
	relayerAddr           address.Address
	validatorContractAddr address.Address

	client                 iotex.AuthedClient
	validatorContract      iotex.Contract
	validatorContractABI   abi.ABI
	witnessListContract    iotex.Contract
	witnessListContractABI abi.ABI
	witnesses              map[string]bool
}

// NewTransferValidatorOnIoTeX creates a new TransferValidator on IoTeX
func NewTransferValidatorOnIoTeX(
	client iotex.AuthedClient,
	privateKey *ecdsa.PrivateKey,
	validatorContractAddr address.Address,
) (TransferValidator, error) {
	validatorContractIoAddr, err := address.FromBytes(validatorContractAddr.Bytes())
	if err != nil {
		return nil, err
	}
	validatorABI, err := abi.JSON(strings.NewReader(contract.TransferValidatorABI))
	if err != nil {
		return nil, err
	}
	validatorContract := client.Contract(validatorContractAddr, validatorABI)

	data, err := validatorContract.Read("witnessList").Call(context.Background())
	if err != nil {
		return nil, err
	}
	witnessContractAddr := common.Address{}
	if validatorABI.Unpack(&witnessContractAddr, "witnessList", data.Raw); err != nil {
		return nil, err
	}
	witnessContractIoAddr, err := address.FromBytes(witnessContractAddr.Bytes())
	if err != nil {
		return nil, err
	}
	witnessContractABI, err := abi.JSON(strings.NewReader(contract.AddressListABI))
	if err != nil {
		return nil, err
	}
	relayerAddr, err := address.FromBytes(crypto.PubkeyToAddress(privateKey.PublicKey).Bytes())
	if err != nil {
		return nil, err
	}

	return &transferValidatorOnIoTeX{
		gasLimit: 2000000,
		gasPrice: big.NewInt(unit.Qev),

		privateKey:            privateKey,
		relayerAddr:           relayerAddr,
		validatorContractAddr: validatorContractIoAddr,

		client:                 client,
		validatorContract:      validatorContract,
		validatorContractABI:   validatorABI,
		witnessListContract:    client.Contract(witnessContractIoAddr, witnessContractABI),
		witnessListContractABI: witnessContractABI,
	}, nil
}

func (tv *transferValidatorOnIoTeX) Address() common.Address {
	tv.mu.RLock()
	defer tv.mu.RUnlock()

	return common.BytesToAddress(tv.validatorContractAddr.Bytes())
}

func (tv *transferValidatorOnIoTeX) refresh() error {
	witnesses := []common.Address{}
	countData, err := tv.witnessListContract.Read("count").Call(context.Background())
	if err != nil {
		return err
	}
	count := big.NewInt(0)
	if err := countData.Unmarshal(&count); err != nil {
		return err
	}
	offset := big.NewInt(0)
	limit := uint8(10)
	for offset.Cmp(count) < 0 {
		data, err := tv.witnessListContract.Read("getActiveItems", offset, limit).Call(context.Background())
		if err != nil {
			return err
		}
		ret := new(struct {
			Count *big.Int
			Items []common.Address
		})
		if err := tv.witnessListContractABI.Unpack(ret, "getActiveItems", data.Raw); err != nil {
			return err
		}
		witnesses = append(witnesses, ret.Items[:int(ret.Count.Int64())]...)
		offset.Add(offset, big.NewInt(int64(limit)))
	}
	log.Println("refresh Witnesses on IoTeX")
	activeWitnesses := make(map[string]bool)
	for _, w := range witnesses {
		addr, err := address.FromBytes(w.Bytes())
		if err != nil {
			return err
		}
		log.Println("\t" + addr.String())
		activeWitnesses[w.Hex()] = true
	}
	tv.witnesses = activeWitnesses

	return nil
}

func (tv *transferValidatorOnIoTeX) isActiveWitness(witness common.Address) bool {
	val, ok := tv.witnesses[witness.Hex()]

	return ok && val
}

// Check returns true if a transfer has been settled
func (tv *transferValidatorOnIoTeX) Check(transfer *Transfer) (StatusOnChainType, error) {
	tv.mu.RLock()
	defer tv.mu.RUnlock()
	accountMeta, err := tv.relayerAccountMeta()
	if err != nil {
		return StatusOnChainUnknown, err
	}
	settleHeightData, err := tv.validatorContract.Read("settles", transfer.id).Call(context.Background())
	if err != nil {
		return StatusOnChainUnknown, err
	}
	settleHeight := big.NewInt(0)
	if err := tv.validatorContractABI.Unpack(&settleHeight, "settles", settleHeightData.Raw); err != nil {
		return StatusOnChainUnknown, err
	}
	if settleHeight.Cmp(big.NewInt(0)) > 0 {
		return StatusOnChainSettled, nil
	}
	response, err := tv.client.API().GetReceiptByAction(context.Background(), &iotexapi.GetReceiptByActionRequest{})
	if err != nil {
		return StatusOnChainUnknown, err
	}
	if response != nil {
		// no matter what the receipt status is, mark the validation as failure
		return StatusOnChainRejected, nil
	}
	if transfer.nonce <= accountMeta.Nonce {
		return StatusOnChainNonceOverwritten, nil
	}

	return StatusOnChainNotConfirmed, nil
}

// Submit submits validation for a transfer
func (tv *transferValidatorOnIoTeX) Submit(transfer *Transfer, witnesses []*Witness) (common.Hash, uint64, error) {
	tv.mu.Lock()
	defer tv.mu.Unlock()

	if err := tv.refresh(); err != nil {
		return common.Hash{}, 0, err
	}
	signatures := []byte{}
	numOfValidSignatures := 0
	for _, witness := range witnesses {
		if !tv.isActiveWitness(witness.addr) {
			addr, err := address.FromBytes(witness.addr.Bytes())
			if err != nil {
				return common.Hash{}, 0, err
			}
			log.Printf("witness %s is inactive\n", addr.String())
			continue
		}
		signatures = append(signatures, witness.signature...)
		numOfValidSignatures++
	}
	if numOfValidSignatures*3 <= len(tv.witnesses)*2 {
		return common.Hash{}, 0, errInsufficientWitnesses
	}
	accountMeta, err := tv.relayerAccountMeta()
	if err != nil {
		return common.Hash{}, 0, errors.Wrapf(err, "failed to get account of %s", tv.relayerAddr.String())
	}
	balance, ok := big.NewInt(0).SetString(accountMeta.Balance, 10)
	if !ok {
		return common.Hash{}, 0, errors.Wrapf(err, "failed to convert balance %s of account %s", accountMeta.Balance, tv.relayerAddr.String())
	}
	if balance.Cmp(new(big.Int).Mul(tv.gasPrice, new(big.Int).SetUint64(tv.gasLimit))) < 0 {
		util.Alert("IOTX native balance has dropped to " + balance.String() + ", please refill account for gas " + tv.relayerAddr.String())
	}

	actionHash, err := tv.validatorContract.Execute(
		"submit",
		transfer.cashier,
		transfer.token,
		new(big.Int).SetUint64(transfer.index),
		transfer.sender,
		transfer.recipient,
		transfer.amount,
		signatures,
	).SetGasPrice(tv.gasPrice).
		SetGasLimit(tv.gasLimit).
		SetNonce(accountMeta.Nonce + 1).
		Call(context.Background())
	if err != nil {
		return common.Hash{}, 0, err
	}

	return common.BytesToHash(actionHash[:]), accountMeta.Nonce + 1, nil
}

func (tv *transferValidatorOnIoTeX) relayerAccountMeta() (*iotextypes.AccountMeta, error) {
	response, err := tv.client.API().GetAccount(context.Background(), &iotexapi.GetAccountRequest{
		Address: tv.relayerAddr.String(),
	})
	if err != nil {
		return nil, err
	}
	return response.AccountMeta, nil
}
