package main

import (
	"container/list"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const (
	blocksInMonth = 4320
	// Mined in Jan 2019. Reasonable start time for lighting network real adoption.
	startBlock = 560000
)

func main() {
	// Connect to local bitcoin core RPC server using HTTP POST mode.
	connCfg := &rpcclient.ConnConfig{
		Host:         "localhost:8332",
		User:         "admin",
		Pass:         "admin",
		HTTPPostMode: true, // Bitcoin core only supports HTTP POST mode
		DisableTLS:   true, // Bitcoin core does not provide TLS by default
	}
	// Notice the notification parameter is nil since notifications are
	// not supported in HTTP POST mode.
	client, err := rpcclient.New(connCfg, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Shutdown()
	// randomDebug(*client)

	// Get the current block count.
	best_height, err := client.GetBlockCount()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Best height: %d", best_height)

	// We aim at estiamting the number of active private channels in the lightning network.
	// Our approach is as follows:
	// 1) Count the number of channel closing transactions in a random month (we can't tell
	//    for sure if a transaction is a channel close specially for private channels but we try hard).
	// 2) Correlate some of the channel closes to known public channels. Rest are likely private
	//    channels closes.
	// 3) Get the ratio of private channel closes to public ones.
	// 4) Repeat step 1-3 a few times and average the resulting ratios.
	// 5) Assuming the resultant ratio is that same as the ratio of active private channels to public ones
	//    we multiply the ratio by the currently known number of active lightning channels to get our result.
	const estimateTrials = 10
	private_to_public_channel_ratio_estimates := list.New()
	for tr := 0; tr < estimateTrials; tr++ {
		block_height := rand.Intn(int(best_height-startBlock)) + startBlock
		log.Println(block_height)
		block_hash, err := client.GetBlockHash(int64(block_height))
		if err != nil {
			log.Fatal(err)
		}
		channel_close_tx_cnt := 0.0
		public_channel_close_tx_cnt := 0.0
		public_channel_cap := int64(0)
		for i := 0; i < blocksInMonth; i++ {
			log.Println(i, public_channel_close_tx_cnt, channel_close_tx_cnt)
			wire_block, err := client.GetBlock(block_hash)
			if err != nil {
				log.Fatal(err)
			}
			for _, tx := range wire_block.Transactions {
				isCloseTx, channelCapacity := isLikelyChannelCloseTx(*tx, *client)
				if isCloseTx {
					channel_close_tx_cnt++
					if isPublicChannel(tx.TxIn[0].PreviousOutPoint.String()) {
						// log.Println("Acual chane ", tx.TxHash())
						public_channel_close_tx_cnt++
						public_channel_cap += channelCapacity
					} else {
						// log.Println("Channel close? ", tx.TxHash())
					}
				}
			}
			block_hash = &wire_block.Header.PrevBlock
		}
		if public_channel_close_tx_cnt > 0 {
			log.Println(channel_close_tx_cnt - public_channel_close_tx_cnt)
			log.Println(public_channel_close_tx_cnt)
			ratio := float64(channel_close_tx_cnt-public_channel_close_tx_cnt) / float64(public_channel_close_tx_cnt)
			private_to_public_channel_ratio_estimates.PushBack(ratio)
		}
	}
	avg_ratio := 0.0
	for e := private_to_public_channel_ratio_estimates.Front(); e != nil; e = e.Next() {
		avg_ratio += e.Value.(float64)
	}
	avg_ratio /= estimateTrials
	// Read for 1ml.com.
	// Can be queried from local lnd node getnetworkinfo RPC. Mine however returns much lower number of channels :/
	currentPublicChannelCount := 82182
	log.Println("Estimated private channel count", int(float64(currentPublicChannelCount)*avg_ratio))
}

// Returns whether the given Tx is likely to be a lightning channel close TX along
// with channel capacity else 0.
func isLikelyChannelCloseTx(tx wire.MsgTx, client rpcclient.Client) (bool, int64) {
	// TX must have only one input and at most two outputs.
	if len(tx.TxIn) != 1 || len(tx.TxOut) > 2 || len(tx.TxIn[0].SignatureScript) != 0 ||
		!tx.HasWitness() {
		return false, 0
	}
	pkScript, err := txscript.ComputePkScript(tx.TxIn[0].SignatureScript, tx.TxIn[0].Witness)
	if err != nil {
		log.Println("Failed to get PkScript!", tx.TxHash())
		return false, 0
	}
	// Right script type and sig count.
	if pkScript.Class() != txscript.WitnessV0ScriptHashTy ||
		txscript.GetWitnessSigOpCount(tx.TxIn[0].SignatureScript,
			pkScript.Script(), tx.TxIn[0].Witness) != 2 {
		return false, 0
	}
	scriptInfo, err := txscript.CalcScriptInfo(tx.TxIn[0].SignatureScript, pkScript.Script(),
		tx.TxIn[0].Witness, true, true)
	if scriptInfo.ExpectedInputs != 4 || scriptInfo.NumInputs != 4 {
		return false, 0
	}
	// All outputs should be segwit, not sure about this but majority of channels are created
	// from wallets supporting segwit.
	for _, txout := range tx.TxOut {
		outputScriptClass := txscript.GetScriptClass(txout.PkScript)
		if outputScriptClass != txscript.WitnessV0ScriptHashTy &&
			outputScriptClass != txscript.WitnessV0PubKeyHashTy {
			return false, 0
		}
	}
	channelOpenTx, err := client.GetRawTransaction(&tx.TxIn[0].PreviousOutPoint.Hash)
	if err != nil {
		log.Println("Failed to get source Tx!")
		return false, 0
	}
	channelCapacity := channelOpenTx.MsgTx().TxOut[tx.TxIn[0].PreviousOutPoint.Index].Value
	// Majority of channels aren't larger than 0.5 btc
	if channelCapacity > 50000000 {
		return false, 0
	}
	return true, channelCapacity
}

// Given string representation of the output supposedly opened the channel, checks 1ml.com for
// the existence of said channel. Input is in the form of txid:index .
func isPublicChannel(channelFundingOutput string) bool {
	postParam := url.Values{}
	postParam.Add("q", channelFundingOutput)
	postParam.Add("type", "channel")
	resp, err := http.PostForm("https://1ml.com/search", postParam)
	if err != nil {
		log.Println(err.Error())
		return false
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	bodyString := string(body)
	// A hack based on insepcting the returned HTML.
	hasOuput := strings.Contains(bodyString, "<span class=\"selectable\">"+channelFundingOutput+"</span>")
	return resp.StatusCode == 200 && hasOuput
}

func randomDebug(client rpcclient.Client) {
	block_hash, err := client.GetBlockHash(567520)
	if err != nil {
		log.Fatal(err)
	}
	wire_block, err := client.GetBlock(block_hash)
	if err != nil {
		log.Fatal(err)
	}
	for _, tx := range wire_block.Transactions {
		if tx.TxHash().String() == "c9714be517c92e95710f6fdae8992f6a7f6f64b4c7bb5bd2b65b5c3400a328e8" { //"37fc0457c8a05f1448fdcaa386e9b9f7dcb5bbb7144a595e2091a4b3380db44d" {

			log.Print(len(tx.TxIn[0].SignatureScript))
			pkScript, err := txscript.ComputePkScript(tx.TxIn[0].SignatureScript, tx.TxIn[0].Witness)
			if err != nil {
				log.Println("a7a")
			}
			log.Println(txscript.GetScriptClass(tx.TxOut[0].PkScript))
			log.Println("witness sig ops", txscript.GetWitnessSigOpCount(tx.TxIn[0].SignatureScript, pkScript.Script(), tx.TxIn[0].Witness))
			log.Println("script op cnt", txscript.GetSigOpCount(pkScript.Script()))
			s_i, err := txscript.CalcScriptInfo(tx.TxIn[0].SignatureScript, pkScript.Script(), tx.TxIn[0].Witness, true, true)
			log.Println("script info", s_i.ExpectedInputs, s_i.NumInputs, s_i.SigOps)
			log.Print(txscript.GetScriptClass(tx.TxOut[0].PkScript) == txscript.WitnessV0ScriptHashTy)

			log.Println("public chan", isPublicChannel(tx.TxIn[0].PreviousOutPoint.String()))
			isCloseTx, channelCapacity := isLikelyChannelCloseTx(*tx, client)
			log.Println(isCloseTx, channelCapacity)
			log.Println((3 - 2) / (2 * 1.0))
			os.Exit(3)
		}
	}
}
