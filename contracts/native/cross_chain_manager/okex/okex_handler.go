/*
 * Copyright (C) 2021 The poly network Authors
 * This file is part of The poly network library.
 *
 * The  poly network  is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Lesser General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * The  poly network  is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Lesser General Public License for more details.
 * You should have received a copy of the GNU Lesser General Public License
 * along with The poly network .  If not, see <http://www.gnu.org/licenses/>.
 */
package okex

import (
	"bytes"
	"fmt"

	"github.com/cosmos/cosmos-sdk/store/rootmulti"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/native"
	scom "github.com/ethereum/go-ethereum/contracts/native/cross_chain_manager/common"
	"github.com/ethereum/go-ethereum/contracts/native/governance/side_chain_manager"
	"github.com/ethereum/go-ethereum/contracts/native/header_sync/okex"
	"github.com/ethereum/go-ethereum/contracts/native/utils"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/polynetwork/poly/common"
	"github.com/tendermint/tendermint/crypto/merkle"
)

type OKHandler struct{}

func NewHandler() *OKHandler {
	return &OKHandler{}
}

type CosmosProofValue struct {
	Kp    string
	Value []byte
}

var (
	KeyPrefixStorage = []byte{0x05}
)

func (this *OKHandler) MakeDepositProposal(service *native.NativeContract) (*scom.MakeTxParam, error) {
	ctx := service.ContractRef().CurrentContext()
	params := &scom.EntranceParam{}
	if err := utils.UnpackMethod(scom.ABI, scom.MethodImportOuterTransfer, params, ctx.Payload); err != nil {
		return nil, err
	}
	info, err := okex.GetEpochSwitchInfo(service, params.SourceChainID)
	if err != nil {
		return nil, fmt.Errorf("okex MakeDepositProposal, failed to get epoch switching height: %v", err)
	}
	if info.Height > int64(params.Height) {
		return nil, fmt.Errorf("okex MakeDepositProposal, the height %d of header is lower than epoch "+
			"switching height %d", params.Height, info.Height)
	}

	if len(params.HeaderOrCrossChainMsg) == 0 {
		return nil, fmt.Errorf("you must commit the header used to verify transaction's proof and get none")
	}
	cdc := okex.NewCDC()
	var myHeader okex.CosmosHeader
	if err := cdc.UnmarshalBinaryBare(params.HeaderOrCrossChainMsg, &myHeader); err != nil {
		return nil, fmt.Errorf("okex MakeDepositProposal, unmarshal okex header failed: %v", err)
	}
	if myHeader.Header.Height != int64(params.Height) {
		return nil, fmt.Errorf("okex MakeDepositProposal, "+
			"height of your header is %d not equal to %d in parameter", myHeader.Header.Height, params.Height)
	}
	if err = okex.VerifyCosmosHeader(&myHeader, info); err != nil {
		return nil, fmt.Errorf("okex MakeDepositProposal, failed to verify okex header: %v", err)
	}
	if !bytes.Equal(myHeader.Header.ValidatorsHash, myHeader.Header.NextValidatorsHash) &&
		myHeader.Header.Height > info.Height {
		okex.PutEpochSwitchInfo(service, params.SourceChainID, &okex.CosmosEpochSwitchInfo{
			Height:             myHeader.Header.Height,
			BlockHash:          myHeader.Header.Hash(),
			NextValidatorsHash: myHeader.Header.NextValidatorsHash,
			ChainID:            myHeader.Header.ChainID,
		})
	}

	var proofValue CosmosProofValue
	if err = cdc.UnmarshalBinaryBare(params.Extra, &proofValue); err != nil {
		return nil, fmt.Errorf("okex MakeDepositProposal, unmarshal proof value err: %v", err)
	}
	var proof merkle.Proof
	err = cdc.UnmarshalBinaryBare(params.Proof, &proof)
	if err != nil {
		return nil, fmt.Errorf("okex MakeDepositProposal, unmarshal proof err: %v", err)
	}
	sideChain, err := side_chain_manager.GetSideChain(service, params.SourceChainID)
	if err != nil {
		return nil, fmt.Errorf("okex MakeDepositProposal, side_chain_manager.GetSideChain error: %v", err)
	}
	if len(proof.Ops) != 2 {
		return nil, fmt.Errorf("proof size wrong")
	}
	if len(proof.Ops[0].Key) != 1+ethcommon.HashLength+ethcommon.AddressLength {
		return nil, fmt.Errorf("storage key length not correct")
	}
	if !bytes.HasPrefix(proof.Ops[0].Key, append(KeyPrefixStorage, sideChain.CCMCAddress...)) {
		return nil, fmt.Errorf("storage key not from ccmc")
	}
	if !bytes.Equal(proof.Ops[1].Key, []byte("evm")) {
		return nil, fmt.Errorf("wrong module for proof")
	}
	if len(proofValue.Kp) == 0 {
		return nil, fmt.Errorf("Cosmos MakeDepositProposal, Kp is nil")
	}

	prt := rootmulti.DefaultProofRuntime()
	err = prt.VerifyValue(&proof, myHeader.Header.AppHash, proofValue.Kp, ethcrypto.Keccak256(proofValue.Value))
	if err != nil {
		return nil, fmt.Errorf("Cosmos MakeDepositProposal, proof error: %s", err)
	}
	data := common.NewZeroCopySource(proofValue.Value)
	txParam := new(scom.MakeTxParam)
	if err := txParam.Deserialization(data); err != nil {
		return nil, fmt.Errorf("Cosmos MakeDepositProposal, deserialize merkleValue error:%s", err)
	}
	if err := scom.CheckDoneTx(service, txParam.CrossChainID, params.SourceChainID); err != nil {
		return nil, fmt.Errorf("Cosmos MakeDepositProposal, check done transaction error:%s", err)
	}
	if err := scom.PutDoneTx(service, txParam.CrossChainID, params.SourceChainID); err != nil {
		return nil, fmt.Errorf("Cosmos MakeDepositProposal, PutDoneTx error:%s", err)
	}
	return txParam, nil
}
