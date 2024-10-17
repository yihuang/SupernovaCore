// Copyright (c) 2020 The Meter.io developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/beevik/ntp"
	cmtcfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/privval"
	"github.com/cometbft/cometbft/proxy"
	cmttypes "github.com/cometbft/cometbft/types"

	db "github.com/cometbft/cometbft-db"
	cmtproxy "github.com/cometbft/cometbft/proxy"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nat"
	"github.com/meterio/supernova/api"
	"github.com/meterio/supernova/block"
	"github.com/meterio/supernova/chain"
	"github.com/meterio/supernova/consensus"
	"github.com/meterio/supernova/genesis"
	"github.com/meterio/supernova/libs/cache"
	"github.com/meterio/supernova/libs/co"
	"github.com/meterio/supernova/libs/comm"
	"github.com/meterio/supernova/p2psrv"
	"github.com/meterio/supernova/txpool"
	"github.com/meterio/supernova/types"
	"github.com/pkg/errors"
)

var (
	GlobNodeInst           *Node
	errCantExtendBestBlock = errors.New("can't extend best block")
	genesisDocHashKey      = []byte("genesisDocHash")
)

func LoadGenesisDoc(
	mainDB db.DB,
	genesisDocProvider types.GenesisDocProvider,
) (*types.GenesisDoc, error) { // originally, LoadStateFromDBOrGenesisDocProvider
	// Get genesis doc hash
	genDocHash, err := mainDB.Get(genesisDocHashKey)
	if err != nil {
		return nil, fmt.Errorf("error retrieving genesis doc hash: %w", err)
	}
	csGenDoc, err := genesisDocProvider()
	if err != nil {
		return nil, err
	}

	if err = csGenDoc.GenesisDoc.ValidateAndComplete(); err != nil {
		return nil, fmt.Errorf("error in genesis doc: %w", err)
	}

	if len(genDocHash) == 0 {
		// Save the genDoc hash in the store if it doesn't already exist for future verification
		if err = mainDB.SetSync(genesisDocHashKey, csGenDoc.Sha256Checksum); err != nil {
			return nil, fmt.Errorf("failed to save genesis doc hash to db: %w", err)
		}
	} else {
		if !bytes.Equal(genDocHash, csGenDoc.Sha256Checksum) {
			return nil, errors.New("genesis doc hash in db does not match loaded genesis doc")
		}
	}

	return csGenDoc.GenesisDoc, nil
}

type Node struct {
	goes          co.Goes
	config        *cmtcfg.Config
	ctx           context.Context
	genesisDoc    *types.GenesisDoc      // initial validator set
	privValidator cmttypes.PrivValidator // local node's validator key

	apiServer *api.APIServer

	// network
	nodeKey *types.NodeKey // our node privkey
	reactor *consensus.Reactor

	chain       *chain.Chain
	txPool      *txpool.TxPool
	txStashPath string
	comm        *comm.Communicator
	logger      *slog.Logger

	proxyApp cmtproxy.AppConns
}

func NewNode(
	config *cmtcfg.Config,
	privValidator *privval.FilePV,
	nodeKey *types.NodeKey,
	clientCreator cmtproxy.ClientCreator,
	genesisDocProvider types.GenesisDocProvider,
	dbProvider cmtcfg.DBProvider,
) *Node {

	ctx := context.Background()
	InitLogger(config)

	slog.Info("Meter Start ...")
	mainDB, err := dbProvider(&cmtcfg.DBContext{ID: "maindb", Config: config})

	genDoc, err := LoadGenesisDoc(mainDB, genesisDocProvider)
	if err != nil {
		panic(err)
	}
	gene := genesis.NewGenesis(genDoc)

	chain := InitChain(gene, mainDB)

	// if flattern index start is not set, or pruning is not complete
	// start the pruning routine right now

	blsMaster := types.NewBlsMasterWithSecretBytes(privValidator.Key.PrivKey.Bytes())

	// set magic
	sum := sha256.Sum256([]byte(fmt.Sprintf(config.BaseConfig.Moniker, config.BaseConfig.Version)))

	// Split magic to p2p_magic and consensus_magic
	copy(p2pMagic[:], sum[:4])

	txPool := txpool.New(chain, txpool.DefaultTxPoolOptions)
	defer func() { slog.Info("closing tx pool..."); txPool.Close() }()

	// Create the proxyApp and establish connections to the ABCI app (consensus, mempool, query).
	proxyApp, err := createAndStartProxyAppConns(clientCreator, cmtproxy.NopMetrics())
	if err != nil {
		panic(err)
	}

	var BootstrapNodes []*enode.Node
	p2pOpts := &p2psrv.Options{
		Name:           types.MakeName(config.BaseConfig.Moniker, config.BaseConfig.Version),
		PrivateKey:     nodeKey.PrivateKey(),
		MaxPeers:       config.P2P.MaxNumInboundPeers,
		ListenAddr:     "0.0.0.0:11235", // config.P2P.ListenAddress,
		BootstrapNodes: BootstrapNodes,
		NAT:            nat.Any(),
	}
	comm := comm.NewCommunicator(ctx, chain, txPool, p2pMagic, p2pOpts, config.RootDir)

	reactor := consensus.NewConsensusReactor(config, chain, comm, txPool, blsMaster, proxyApp)

	pubkey, err := privValidator.GetPubKey()

	apiAddr := ":8670"
	apiServer := api.NewAPIServer(apiAddr, gene.ChainId, config.BaseConfig.Version, chain, txPool, reactor, pubkey.Bytes(), comm)

	bestBlock := chain.BestBlock()

	fmt.Printf(`Starting %v
    Magic           [ %v p2p & consensus ]
    Network         [ %v %v ]    
    Best block      [ %v #%v @%v ]
    Forks           [ %v ]
    PubKey          [ %v ]
    API portal      [ %v ]
`,
		types.MakeName(config.BaseConfig.Moniker, config.BaseConfig.Version),
		hex.EncodeToString(p2pMagic[:]),
		gene.ID(), gene.Name,
		bestBlock.ID(), bestBlock.Number(), time.Unix(int64(bestBlock.Timestamp()), 0),
		types.GetForkConfig(gene.ID()),
		hex.EncodeToString(pubkey.Bytes()),
		apiAddr)

	node := &Node{
		ctx:           ctx,
		config:        config,
		genesisDoc:    genDoc,
		privValidator: privValidator,
		nodeKey:       nodeKey,
		apiServer:     apiServer,
		reactor:       reactor,
		chain:         chain,
		txPool:        txPool,
		comm:          comm,
		logger:        slog.With("pkg", "node"),
		proxyApp:      proxyApp,
	}

	return node
}

func createAndStartProxyAppConns(clientCreator cmtproxy.ClientCreator, metrics *cmtproxy.Metrics) (proxy.AppConns, error) {
	proxyApp := proxy.NewAppConns(clientCreator, metrics)
	if err := proxyApp.Start(); err != nil {
		return nil, fmt.Errorf("error starting proxy app connections: %v", err)
	}
	return proxyApp, nil
}

func (n *Node) Start() error {
	n.logger.Info("Node Start")
	n.comm.Start()
	n.comm.Sync(n.handleBlockStream)

	n.goes.Go(func() { n.apiServer.Start(n.ctx) })
	n.goes.Go(func() { n.houseKeeping(n.ctx) })
	// n.goes.Go(func() { n.txStashLoop(n.ctx) })
	n.goes.Go(func() { n.reactor.Start(n.ctx) })

	n.goes.Wait()
	return nil
}

func (n *Node) Stop() error {
	n.comm.Stop()
	return nil
}

func (n *Node) handleBlockStream(ctx context.Context, stream <-chan *block.EscortedBlock) (err error) {
	n.logger.Debug("start to process block stream")
	defer n.logger.Debug("process block stream done", "err", err)
	var stats blockStats
	startTime := mclock.Now()

	report := func(block *block.Block, pending int) {
		n.logger.Info(fmt.Sprintf("imported blocks (%v) ", stats.processed), stats.LogContext(block.Header(), pending)...)
		stats = blockStats{}
		startTime = mclock.Now()
	}

	var blk *block.EscortedBlock
	for blk = range stream {
		n.logger.Debug("handle block", "block", blk.Block.ID().ToBlockShortID())
		if isTrunk, err := n.processBlock(blk.Block, blk.EscortQC, &stats); err != nil {
			if err == errCantExtendBestBlock {
				best := n.chain.BestBlock()
				n.logger.Warn("process block failed", "num", blk.Block.Number(), "id", blk.Block.ID(), "best", best.Number(), "err", err.Error())
			} else {
				n.logger.Error("process block failed", "num", blk.Block.Number(), "id", blk.Block.ID(), "err", err.Error())
			}
			return err
		} else if isTrunk {
			// this processBlock happens after consensus SyncDone, need to broadcast
			if n.reactor.SyncDone {
				n.comm.BroadcastBlock(blk)
			}
		}

		if stats.processed > 0 &&
			mclock.Now()-startTime > mclock.AbsTime(time.Second*2) {
			report(blk.Block, len(stream))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if blk != nil && stats.processed > 0 {
		report(blk.Block, len(stream))
	}
	return nil
}

func (n *Node) houseKeeping(ctx context.Context) {
	n.logger.Debug("enter house keeping")
	defer n.logger.Debug("leave house keeping")

	var scope event.SubscriptionScope
	defer scope.Close()

	newBlockCh := make(chan *comm.NewBlockEvent)
	scope.Track(n.comm.SubscribeBlock(newBlockCh))

	futureTicker := time.NewTicker(time.Duration(types.BlockInterval) * time.Second)
	defer futureTicker.Stop()

	connectivityTicker := time.NewTicker(time.Second)
	defer connectivityTicker.Stop()

	var noPeerTimes int

	futureBlocks := cache.NewRandCache(32)

	for {
		select {
		case <-ctx.Done():
			return
		case newBlock := <-newBlockCh:
			var stats blockStats

			if isTrunk, err := n.processBlock(newBlock.Block, newBlock.EscortQC, &stats); err != nil {
				if consensus.IsFutureBlock(err) ||
					(consensus.IsParentMissing(err) && futureBlocks.Contains(newBlock.Block.Header().ParentID)) {
					n.logger.Debug("future block added", "id", newBlock.Block.ID())
					futureBlocks.Set(newBlock.Block.ID(), newBlock)
				}
			} else if isTrunk {
				n.comm.BroadcastBlock(newBlock.EscortedBlock)
				// n.logger.Info(fmt.Sprintf("imported blocks (%v)", stats.processed), stats.LogContext(newBlock.Block.Header())...)
			}
		case <-futureTicker.C:
			// process future blocks
			var blocks []*block.EscortedBlock
			futureBlocks.ForEach(func(ent *cache.Entry) bool {
				blocks = append(blocks, ent.Value.(*block.EscortedBlock))
				return true
			})
			sort.Slice(blocks, func(i, j int) bool {
				return blocks[i].Block.Number() < blocks[j].Block.Number()
			})
			var stats blockStats
			for i, block := range blocks {
				if isTrunk, err := n.processBlock(block.Block, block.EscortQC, &stats); err == nil || consensus.IsKnownBlock(err) {
					n.logger.Debug("future block consumed", "id", block.Block.ID())
					futureBlocks.Remove(block.Block.ID())
					if isTrunk {
						n.comm.BroadcastBlock(block)
					}
				}

				if stats.processed > 0 && i == len(blocks)-1 {
					// n.logger.Info(fmt.Sprintf("imported blocks (%v)", stats.processed), stats.LogContext(block.Header())...)
				}
			}
		case <-connectivityTicker.C:
			if n.comm.PeerCount() == 0 {
				noPeerTimes++
				if noPeerTimes > 30 {
					noPeerTimes = 0
					go checkClockOffset()
				}
			} else {
				noPeerTimes = 0
			}
		}
	}
}

// func (n *Node) txStashLoop(ctx context.Context) {
// 	n.logger.Debug("enter tx stash loop")
// 	defer n.logger.Debug("leave tx stash loop")

// 	db, err := lvldb.New(n.txStashPath, lvldb.Options{})
// 	if err != nil {
// 		n.logger.Error("create tx stash", "err", err)
// 		return
// 	}
// 	defer db.Close()

// 	stash := newTxStash(db, 1000)

// 	{
// 		txs := stash.LoadAll()
// 		bestBlock := n.chain.BestBlock()
// 		n.txPool.Fill(txs, func(txID []byte) bool {
// 			if _, err := n.chain.GetTransactionMeta(txID, bestBlock.ID()); err != nil {
// 				return false
// 			} else {
// 				return true
// 			}
// 		})
// 		n.logger.Debug("loaded txs from stash", "count", len(txs))
// 	}

// 	var scope event.SubscriptionScope
// 	defer scope.Close()

// 	txCh := make(chan *txpool.TxEvent)
// 	scope.Track(n.txPool.SubscribeTxEvent(txCh))
// 	for {
// 		select {
// 		case <-ctx.Done():
// 			return
// 		case txEv := <-txCh:
// 			// skip executables
// 			if txEv.Executable != nil && *txEv.Executable {
// 				continue
// 			}
// 			// only stash non-executable txs
// 			if err := stash.Save(txEv.Tx); err != nil {
// 				n.logger.Warn("stash tx", "id", txEv.Tx.Hash(), "err", err)
// 			} else {
// 				n.logger.Debug("stashed tx", "id", txEv.Tx.Hash())
// 			}
// 		}
// 	}
// }

func (n *Node) processBlock(blk *block.Block, escortQC *block.QuorumCert, stats *blockStats) (bool, error) {
	now := uint64(time.Now().Unix())

	best := n.chain.BestBlock()
	if !bytes.Equal(best.ID().Bytes(), blk.ParentID().Bytes()) {
		return false, errCantExtendBestBlock
	}
	if blk.Timestamp()+types.BlockInterval > now {
		QCValid := n.reactor.Pacemaker.ValidateQC(blk, escortQC)
		if !QCValid {
			return false, errors.New(fmt.Sprintf("invalid %s on Block %s", escortQC.String(), blk.ID().ToBlockShortID()))
		}
	}
	start := time.Now()
	err := n.reactor.ValidateSyncedBlock(blk, now)
	if time.Since(start) > time.Millisecond*500 {
		n.logger.Debug("slow processed block", "blk", blk.Number(), "elapsed", types.PrettyDuration(time.Since(start)))
	}

	if err != nil {
		switch {
		case consensus.IsKnownBlock(err):
			return false, nil
		case consensus.IsFutureBlock(err) || consensus.IsParentMissing(err):
			return false, nil
		case consensus.IsCritical(err):
			msg := fmt.Sprintf(`failed to process block due to consensus failure \n%v\n`, blk.Header())
			n.logger.Error(msg, "err", err)
		default:
			n.logger.Error("failed to process block", "err", err)
		}
		return false, err
	}
	fork, err := n.commitBlock(blk, escortQC)
	if err != nil {
		if !n.chain.IsBlockExist(err) {
			n.logger.Error("failed to commit block", "err", err)
		}
		return false, err
	}

	stats.UpdateProcessed(1, len(blk.Txs))
	n.processFork(fork)

	// regulate if a kblock is committed
	if blk.IsKBlock() {
		n.logger.Info("synced a KBlock, schedule regulate now", "blk", blk.ID().ToBlockShortID())
		n.reactor.Pacemaker.ScheduleRegulate()
	}

	// end of shortcut
	return len(fork.Trunk) > 0, nil
}

func (n *Node) commitBlock(newBlock *block.Block, escortQC *block.QuorumCert) (*chain.Fork, error) {
	start := time.Now()
	// fmt.Println("Calling AddBlock from node.commitBlock, newBlock=", newBlock.ID())
	fork, err := n.chain.AddBlock(newBlock, escortQC)
	if err != nil {
		return nil, err
	}

	// skip logdb access if no txs
	if len(newBlock.Transactions()) > 0 {
		forkIDs := make([]types.Bytes32, 0, len(fork.Branch))
		for _, header := range fork.Branch {
			forkIDs = append(forkIDs, header.ID())
		}

	}

	if n.reactor.SyncDone {
		n.logger.Info(fmt.Sprintf("* synced %v", newBlock.CompactString()), "txs", len(newBlock.Txs), "epoch", newBlock.Epoch(), "elapsed", types.PrettyDuration(time.Since(start)))
	} else {
		if time.Since(start) > time.Millisecond*500 {
			n.logger.Info(fmt.Sprintf("* slow synced %v", newBlock.CompactString()), "txs", len(newBlock.Txs), "epoch", newBlock.Epoch(), "elapsed", types.PrettyDuration(time.Since(start)))
		}
	}
	return fork, nil
}

func (n *Node) processFork(fork *chain.Fork) {
	if len(fork.Branch) >= 2 {
		trunkLen := len(fork.Trunk)
		branchLen := len(fork.Branch)
		n.logger.Warn(fmt.Sprintf(
			`⑂⑂⑂⑂⑂⑂⑂⑂ FORK HAPPENED ⑂⑂⑂⑂⑂⑂⑂⑂
ancestor: %v
trunk:    %v  %v
branch:   %v  %v`, fork.Ancestor,
			trunkLen, fork.Trunk[trunkLen-1],
			branchLen, fork.Branch[branchLen-1]))
	}
	for _, header := range fork.Branch {
		body, err := n.chain.GetBlockBody(header.ID())
		if err != nil {
			n.logger.Warn("failed to get block body", "err", err, "blockid", header.ID())
			continue
		}
		for _, tx := range body.Txs {
			if err := n.txPool.Add(tx); err != nil {
				n.logger.Debug("failed to add tx to tx pool", "err", err, "id", tx.Hash())
			}
		}
	}
}

func checkClockOffset() {
	resp, err := ntp.Query("ap.pool.ntp.org")
	if err != nil {
		slog.Debug("failed to access NTP", "err", err)
		return
	}
	if resp.ClockOffset > time.Duration(types.BlockInterval)*time.Second/2 {
		slog.Warn("clock offset detected", "offset", types.PrettyDuration(resp.ClockOffset))
	}
}

func (n *Node) IsRunning() bool {
	// FIXME: set correct value
	return true
}
