package core

import (
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"

	"../util"
)

type UTXO struct {
	txMap       [HashSize]byte
	outputIndex uint32
}

/*
 * A block chain contains
 * - a chain of blocks indexed by hash and block index
 * - a set of unspent transaction output
 * - a set of Transactions indexed by tx hash
 */
type Blockchain struct {
	txMap     map[[HashSize]byte]*Transaction /* map of all Transactions in the chain */
	utxoMap   map[UTXO]bool                   /* map of all unspent transaction output (key is not used) */
	blockMap  map[[HashSize]byte]*Block       /* map of all blocks */
	blockList []*Block                        /* list of all blocks */

	difficulty Difficulty

	/* fields to support wallet */
	AddressMap      map[rsa.PublicKey]map[UTXO]bool /* map of all Addresses to their utxo list */
	TransactionPool map[*Transaction]bool           /* all transaction broadcastd by user */
}

func (chain *Blockchain) GetDifficulty() Difficulty {
	return chain.difficulty
}

func (chain *Blockchain) verifyTransaction(tran *Transaction, inputMap map[UTXO]bool) (uint64, error) {
	var totalInput uint64
	var fromAddresses []*rsa.PublicKey
	util.GetTempLogger().Infof("Verify input\n")

	for _, input := range tran.Inputs {
		var utxo UTXO
		utxo.outputIndex = input.OutputIndex
		utxo.txMap = input.PrevtxMap

		/*
		 * Step 1: Verify if the transaction Inputs are unique
		 */
		_, in := inputMap[utxo]
		if in {
			return 0, errors.New("All Inputs must be unique")
		}
		inputMap[utxo] = false

		/*
		 * Step 2: Verify if the UTXO exists in the chain
		 */
		_, inUtxoMap := chain.utxoMap[utxo]
		if !inUtxoMap {
			return 0, errors.New("Cannot find UTXO corresponding to an input in the chain")
		}

		/*
		 * Step 3: Sanity check if the UTXO has a valid transaction
		 */
		tx, intxMap := chain.txMap[utxo.txMap]
		if !intxMap {
			return 0, errors.New("Blockchain is corrupted: cannot find tx")
		}
		if utxo.outputIndex >= uint32(len(tx.Outputs)) {
			return 0, errors.New("Blockchain is corrupted: cannot find utxo")
		}

		totalInput += tx.Outputs[utxo.outputIndex].Value
		fromAddresses = append(fromAddresses, &tx.Outputs[utxo.outputIndex].Address)
	}

	util.GetTempLogger().Infof("Verify signature %v\n", fromAddresses)
	/*
	 * Step 4: Verify signatures
	 */
	err := tran.VerifyTransaction(fromAddresses)
	if err != nil {
		return 0, err
	}

	util.GetTempLogger().Infof("Verify output\n")
	/*
	 * Step 5: Make sure total input <= total output (the gap is the transaction fee)
	 */
	var totalOutput uint64
	for _, output := range tran.Outputs {
		totalOutput += output.Value
	}
	if totalOutput > totalInput {
		return 0, errors.New("The Value of total Outputs exceed that of Inputs")
	}

	return totalInput - totalOutput, nil

}

func (chain *Blockchain) addUTXOToAddress(utxo *UTXO, Address *rsa.PublicKey) {
	m, exist := chain.AddressMap[*Address]
	if !exist {
		m = make(map[UTXO]bool)
		chain.AddressMap[*Address] = m
	}

	m[*utxo] = false
}

func (chain *Blockchain) removeUTXOFromAddress(utxo *UTXO, Address *rsa.PublicKey) {
	m, exist := chain.AddressMap[*Address]
	if !exist {
		return
	}

	delete(m, *utxo)
}

/*
 * Perform the transaction atomically assuming the transaction is valid.
 */
func (chain *Blockchain) performTransaction(tran *Transaction) {
	txMap := sha256.Sum256(tran.GetRawDataToHash())
	chain.txMap[txMap] = tran
	for _, input := range tran.Inputs {
		var utxo UTXO
		utxo.outputIndex = input.OutputIndex
		utxo.txMap = input.PrevtxMap

		delete(chain.utxoMap, utxo)
		tx := chain.txMap[input.PrevtxMap]
		chain.removeUTXOFromAddress(&utxo, &tx.Outputs[utxo.outputIndex].Address)
	}
	for i, output := range tran.Outputs {
		var utxo UTXO
		utxo.outputIndex = uint32(i)
		utxo.txMap = txMap
		chain.utxoMap[utxo] = false
		chain.addUTXOToAddress(&utxo, &output.Address)
	}

	// remove the transaction from the poposal pool
	delete(chain.TransactionPool, tran)
}

func (chain *Blockchain) performMinerTransactionAndAddBlock(block *Block) {
	var utxo UTXO
	utxo.outputIndex = 0
	utxo.txMap = sha256.Sum256(block.Transactions[0].GetRawDataToHash())
	chain.utxoMap[utxo] = false
	chain.txMap[utxo.txMap] = &block.Transactions[0]
	chain.addUTXOToAddress(&utxo, &block.minerAddress)

	chain.blockList = append(chain.blockList, block)
	blockHash := sha256.Sum256(block.getRawDataToHash())
	chain.blockMap[blockHash] = block
}

/*
 * Append the block to the end of the chain.
 * TODO: Support appending the block to a block that is a few blocks ahead of the end
 */
func (chain *Blockchain) AddBlock(block *Block) error {
	if block.prevBlockHash != chain.blockList[len(chain.blockList)-1].hash {
		return errors.New("The prevous block must be the last block in the chain")
	}

	if block.blockIdx != uint64(len(chain.blockList)) {
		return errors.New("Invalid block index")
	}

	if len(block.Transactions) == 0 {
		return errors.New("The Transactions must contain miner's reward as the first transaction")
	}

	if len(block.Transactions[0].Outputs) != 1 {
		return errors.New("Only one miner is allowed in each block")
	}

	util.GetTempLogger().Infof("start verification trans")

	var inputMap map[UTXO]bool
	inputMap = make(map[UTXO]bool)
	var totalFee uint64
	for i, tx := range block.Transactions {
		/* Ignore the first transaction which contains miner's reward */
		if i == 0 {
			continue
		}
		util.GetTempLogger().Infof("Verify tran %s\n", tx.Print())

		fee, error := chain.verifyTransaction(&tx, inputMap)
		if error != nil {
			return error
		}
		totalFee += fee
	}

	/* 100 coins as base award, should be adjusted based on time */
	var minerReward uint64
	minerReward = MinerRewardBase + totalFee
	if block.Transactions[0].Outputs[0].Value > minerReward {
		return errors.New("Miner's reward exceeds base + fee")
	}

	if block.timeStampMs <= chain.GetLatestBlock().timeStampMs {
		return errors.New("Timestamp must be monotonic increasing")
	}

	if !chain.ReachDifficulty(block) {
		return errors.New("The block doesn't meet difficulty")
	}

	util.GetTempLogger().Infof("start perform trans")

	/*
	 * Perform all Transactions
	 */
	for i := range block.Transactions {
		if i == 0 {
			continue
		}
		chain.performTransaction(&block.Transactions[i])
	}

	chain.difficulty.UpdateDifficulty(block.timeStampMs - chain.GetLatestBlock().timeStampMs)
	chain.performMinerTransactionAndAddBlock(block)
	return nil
}

func (chain *Blockchain) GetNLatestBlock(n int) *Block {
	if n > len(chain.blockList) {
		return nil
	}
	return chain.blockList[len(chain.blockList)-n]
}

func (chain *Blockchain) GetLatestBlock() *Block {
	return chain.GetNLatestBlock(1)
}

func (chain *Blockchain) ReachDifficulty(block *Block) bool {
	return chain.difficulty.ReachDifficulty(block.hash)
}

func (chain *Blockchain) RegisterUser(user rsa.PublicKey, utxoMap map[UTXO]bool) {
	chain.AddressMap[user] = utxoMap
}

func (chain *Blockchain) AcceptBroadcastedTransaction(tran *Transaction) {
	chain.TransactionPool[tran] = true
}

/***********************************
 * Wallet related methods
 **********************************/

/*
 * Balance of an Address
 */
func (chain *Blockchain) BalanceOf(Address *rsa.PublicKey) uint64 {
	m, exist := chain.AddressMap[*Address]
	if !exist {
		util.GetBlockchainLogger().Errorf("Address %x disappear from chain\n", *Address)
		return 0
	}

	var balance uint64
	for utxo := range m {
		tx := chain.txMap[utxo.txMap]
		balance += tx.Outputs[utxo.outputIndex].Value
	}
	return balance
}

/*
 * Make a transaction to transfer coins from one account to target Address.
 * Return nil if there is insufficient fund or amount is zero
 * Note that the transaction is unsigned
 */
func (chain *Blockchain) TransferCoin(from *rsa.PublicKey, to *rsa.PublicKey, amount uint64, fee uint64) (*Transaction, error) {
	if amount == 0 {
		return nil, fmt.Errorf("amount needs > 0")
	}

	if chain.BalanceOf(from) < amount {
		return nil, fmt.Errorf("user %s has no enough balance", util.GetShortIdentity(*from))
	}

	fromMap := chain.AddressMap[*from]
	var utxoList []UTXO
	var fromAmount uint64
	for fromUTXO := range fromMap {
		utxoList = append(utxoList, fromUTXO)
		fromTx := chain.txMap[fromUTXO.txMap]
		fromAmount += fromTx.Outputs[fromUTXO.outputIndex].Value

		if fromAmount >= amount+fee {
			break
		}
	}

	var outputLen int
	if amount+fee == fromAmount {
		outputLen = 1
	} else {
		outputLen = 2
	}

	tx := CreateTransaction(len(utxoList), outputLen)
	for i, utxo := range utxoList {
		tx.Inputs[i].OutputIndex = utxo.outputIndex
		tx.Inputs[i].PrevtxMap = utxo.txMap
	}

	tx.Outputs[0].Address = *to
	tx.Outputs[0].Value = amount

	if outputLen == 2 {
		tx.Outputs[1].Address = *from
		tx.Outputs[1].Value = fromAmount - amount - fee
	}

	util.GetBlockchainLogger().Debugf("Constructed transaction %v", tx)
	return &tx, nil
}
