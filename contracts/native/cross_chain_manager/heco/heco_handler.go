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

package heco

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"

	ecommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/native"
	scom "github.com/ethereum/go-ethereum/contracts/native/cross_chain_manager/common"
	"github.com/ethereum/go-ethereum/contracts/native/governance/side_chain_manager"
	"github.com/ethereum/go-ethereum/contracts/native/header_sync/eth"
	"github.com/ethereum/go-ethereum/contracts/native/header_sync/heco"
	"github.com/ethereum/go-ethereum/contracts/native/utils"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	polycomm "github.com/polynetwork/poly/common"
)

// Handler ...
type HecoHandler struct {
}

// NewHandler ...
func NewHecoHandler() *HecoHandler {
	return &HecoHandler{}
}

// MakeDepositProposal ...
func (h *HecoHandler) MakeDepositProposal(service *native.NativeContract) (*scom.MakeTxParam, error) {
	ctx := service.ContractRef().CurrentContext()
	params := &scom.EntranceParam{}
	if err := utils.UnpackMethod(scom.ABI, scom.MethodImportOuterTransfer, params, ctx.Payload); err != nil {
		return nil, err
	}

	sideChain, err := side_chain_manager.GetSideChain(service, params.SourceChainID)
	if err != nil {
		return nil, fmt.Errorf("heco MakeDepositProposal, side_chain_manager.GetSideChain error: %v", err)
	}

	value, err := verifyFromHecoTx(service, params.Proof, params.Extra, params.SourceChainID, params.Height, sideChain)
	if err != nil {
		return nil, fmt.Errorf("heco MakeDepositProposal, verifyFromEthTx error: %s", err)
	}

	if err := scom.CheckDoneTx(service, value.CrossChainID, params.SourceChainID); err != nil {
		return nil, fmt.Errorf("heco MakeDepositProposal, check done transaction error:%s", err)
	}
	if err := scom.PutDoneTx(service, value.CrossChainID, params.SourceChainID); err != nil {
		return nil, fmt.Errorf("heco MakeDepositProposal, PutDoneTx error:%s", err)
	}
	return value, nil
}

func verifyFromHecoTx(native *native.NativeContract, proof, extra []byte, fromChainID uint64, height uint32, sideChain *side_chain_manager.SideChain) (param *scom.MakeTxParam, err error) {
	cheight, err := heco.GetCanonicalHeight(native, fromChainID)
	if err != nil {
		return
	}

	cheight32 := uint32(cheight)

	if cheight32 < height || cheight32-height < uint32(sideChain.BlocksToWait-1) {
		return nil, fmt.Errorf("verifyFromHecoTx, transaction is not confirmed, current height: %d, input height: %d", cheight, height)
	}

	headerWithSum, err := heco.GetCanonicalHeader(native, fromChainID, uint64(height))
	if err != nil {
		return nil, fmt.Errorf("verifyFromHecoTx, GetCanonicalHeader height:%d, error:%s", height, err)
	}

	hecoProof := new(Proof)
	err = json.Unmarshal(proof, hecoProof)
	if err != nil {
		return nil, fmt.Errorf("verifyFromHecoTx, unmarshal proof error:%s", err)
	}

	if len(hecoProof.StorageProofs) != 1 {
		return nil, fmt.Errorf("verifyFromHecoTx, incorrect proof format")
	}

	proofResult, err := verifyMerkleProof(hecoProof, headerWithSum.Header, sideChain.CCMCAddress)
	if err != nil {
		return nil, fmt.Errorf("verifyFromHecoTx, verifyMerkleProof error:%v", err)
	}

	if proofResult == nil {
		return nil, fmt.Errorf("verifyFromHecoTx, verifyMerkleProof failed")
	}

	if !checkProofResult(proofResult, extra) {
		return nil, fmt.Errorf("verifyFromHecoTx, verify proof value hash failed, proof result:%x, extra:%x", proofResult, extra)
	}

	data := polycomm.NewZeroCopySource(extra)
	txParam := new(scom.MakeTxParam)
	if err := txParam.Deserialization(data); err != nil {
		return nil, fmt.Errorf("verifyFromHecoTx, deserialize merkleValue error:%s", err)
	}
	return txParam, nil
}

// Proof ...
type Proof struct {
	Address       string         `json:"address"`
	Balance       string         `json:"balance"`
	CodeHash      string         `json:"codeHash"`
	Nonce         string         `json:"nonce"`
	StorageHash   string         `json:"storageHash"`
	AccountProof  []string       `json:"accountProof"`
	StorageProofs []StorageProof `json:"storageProof"`
}

// StorageProof ...
type StorageProof struct {
	Key   string   `json:"key"`
	Value string   `json:"value"`
	Proof []string `json:"proof"`
}

// ProofAccount ...
type ProofAccount struct {
	Nounce   *big.Int
	Balance  *big.Int
	Storage  ecommon.Hash
	Codehash ecommon.Hash
}

func verifyMerkleProof(hecoProof *Proof, blockData *eth.Header, contractAddr []byte) ([]byte, error) {
	//1. prepare verify account
	nodeList := new(light.NodeList)

	for _, s := range hecoProof.AccountProof {
		p := scom.Replace0x(s)
		nodeList.Put(nil, ecommon.Hex2Bytes(p))
	}
	ns := nodeList.NodeSet()

	addr := ecommon.Hex2Bytes(scom.Replace0x(hecoProof.Address))
	if !bytes.Equal(addr, contractAddr) {
		return nil, fmt.Errorf("verifyMerkleProof, contract address is error, proof address: %s, side chain address: %s", hecoProof.Address, hex.EncodeToString(contractAddr))
	}
	acctKey := crypto.Keccak256(addr)

	//2. verify account proof
	acctVal, err := trie.VerifyProof(blockData.Root, acctKey, ns)
	if err != nil {
		return nil, fmt.Errorf("verifyMerkleProof, verify account proof error:%s", err)
	}

	nounce := new(big.Int)
	_, ok := nounce.SetString(scom.Replace0x(hecoProof.Nonce), 16)
	if !ok {
		return nil, fmt.Errorf("verifyMerkleProof, invalid format of nounce:%s", hecoProof.Nonce)
	}

	balance := new(big.Int)
	_, ok = balance.SetString(scom.Replace0x(hecoProof.Balance), 16)
	if !ok {
		return nil, fmt.Errorf("verifyMerkleProof, invalid format of balance:%s", hecoProof.Balance)
	}

	storageHash := ecommon.HexToHash(scom.Replace0x(hecoProof.StorageHash))
	codeHash := ecommon.HexToHash(scom.Replace0x(hecoProof.CodeHash))

	acct := &ProofAccount{
		Nounce:   nounce,
		Balance:  balance,
		Storage:  storageHash,
		Codehash: codeHash,
	}

	acctrlp, err := rlp.EncodeToBytes(acct)
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(acctrlp, acctVal) {
		return nil, fmt.Errorf("verifyMerkleProof, verify account proof failed, wanted:%v, get:%v", acctrlp, acctVal)
	}

	//3.verify storage proof
	nodeList = new(light.NodeList)
	if len(hecoProof.StorageProofs) != 1 {
		return nil, fmt.Errorf("verifyMerkleProof, invalid storage proof format")
	}

	sp := hecoProof.StorageProofs[0]
	storageKey := crypto.Keccak256(ecommon.HexToHash(scom.Replace0x(sp.Key)).Bytes())

	for _, prf := range sp.Proof {
		nodeList.Put(nil, ecommon.Hex2Bytes(scom.Replace0x(prf)))
	}

	ns = nodeList.NodeSet()
	val, err := trie.VerifyProof(storageHash, storageKey, ns)
	if err != nil {
		return nil, fmt.Errorf("verifyMerkleProof, verify storage proof error:%s", err)
	}

	return val, nil
}

func checkProofResult(result, value []byte) bool {
	var tempBytes []byte
	err := rlp.DecodeBytes(result, &tempBytes)
	if err != nil {
		log.Errorf("checkProofResult, rlp.DecodeBytes error:%s\n", err)
		return false
	}
	//
	var s []byte
	for i := len(tempBytes); i < 32; i++ {
		s = append(s, 0)
	}
	s = append(s, tempBytes...)
	hash := crypto.Keccak256(value)
	return bytes.Equal(s, hash)
}
