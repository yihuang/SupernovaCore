package consensus

import (
	"fmt"
	"time"

	v1 "github.com/cometbft/cometbft/api/cometbft/abci/v1"
	"github.com/meterio/supernova/block"
	"github.com/meterio/supernova/chain"
	"github.com/meterio/supernova/types"
)

func (p *Pacemaker) FinalizeBlockViaABCI(blk *block.Block) error {
	txs := make([][]byte, 0)
	for _, tx := range blk.Txs {
		txs = append(txs, tx)
	}
	res, err := p.executor.FinalizeBlock(&v1.FinalizeBlockRequest{Txs: txs, Height: int64(blk.Number()), Hash: blk.ID().Bytes()})
	if err != nil {
		return err
	}
	// res.AppHash
	// res.TxResults
	blk.BlockHeader.AppHash = res.AppHash
	p.executor.Commit()

	// calculate the next committee
	if len(res.ValidatorUpdates) > 0 {
		nxtVSet, addedValidators, err := p.validatorSetRegistry.Update(blk.Number(), p.epochState.committee, res.ValidatorUpdates, res.Events)
		if err != nil {
			p.logger.Warn("could not update vset registry", "err", err)
			return err
		}
		//stage := blkInfo.Stage

		p.nextEpochState, err = NewPendingEpochState(nxtVSet, p.blsMaster.PubKey, p.epochState.epoch)
		if err != nil {
			p.logger.Error("could not calc pending epoch state", "err", err)
			return err
		}
		p.logger.Info("next epoch state", "incommittee", p.nextEpochState.inCommittee, "epoch", p.nextEpochState.epoch)

		if p.nextEpochState.inCommittee && !p.epochState.inCommittee {
			// if I'm not in current committee but in the next
			// prepare pacemaker for this
		}

		if p.epochState.inCommittee && len(addedValidators) > 0 {
			// if I'm in the current committee, I'm responsible for forwarding messages from now
			p.logger.Info("updated addedValidators", "size", len(addedValidators))
			p.addedValidators = addedValidators
		}
	}

	return nil
}

// finalize the block with its own QC
func (p *Pacemaker) CommitBlock(blk *block.Block, escortQC *block.QuorumCert) error {

	start := time.Now()
	p.logger.Debug("try to finalize block", "block", blk.Oneliner())

	// fmt.Println("Calling AddBlock from consensus_block.commitBlock, newBlock=", blk.ID())
	if blk.Number() <= p.chain.BestBlock().Number() {
		return errKnownBlock
	}
	fork, err := p.chain.AddBlock(blk, escortQC)
	if err != nil {
		if err == chain.ErrBlockExist {
			p.logger.Info("block already exist", "id", blk.ID(), "num", blk.Number())
		} else {
			p.logger.Warn("add block failed ...", "err", err, "id", blk.ID(), "num", blk.Number())
		}
		return err
	}

	err = p.FinalizeBlockViaABCI(blk)
	if err != nil {
		p.logger.Warn("could not finalize via ABCI", "err", err)
		return err
	}

	// unlike processBlock, we do not need to handle fork
	if fork != nil {
		// process fork????
		if len(fork.Branch) > 0 {
			out := fmt.Sprintf("Fork Happened ... fork(Ancestor=%s, Branch=%s), bestBlock=%s", fork.Ancestor.ID().String(), fork.Branch[0].ID().String(), p.chain.BestBlock().ID().String())
			p.logger.Warn(out)
			p.printFork(fork)
			p.ScheduleRegulate()
			return ErrForkHappened
		}
	}

	p.logger.Info(fmt.Sprintf("* committed %v", blk.CompactString()), "txs", len(blk.Txs), "epoch", blk.Epoch(), "elapsed", types.PrettyDuration(time.Since(start)))

	p.lastCommitted = blk
	// broadcast the new block to all peers
	p.communicator.BroadcastBlock(&block.EscortedBlock{Block: blk, EscortQC: escortQC})
	// successfully added the block, update the current hight of consensus

	if blk.IsKBlock() {
		p.logger.Info("committed a KBlock, schedule regulate now", "blk", blk.ID().ToBlockShortID())
		p.ScheduleRegulate()
	}
	return nil
}
