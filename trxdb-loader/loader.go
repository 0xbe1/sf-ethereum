// Copyright 2021 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package trxdb_loader

import (
	"context"
	"fmt"
	"time"

	"github.com/streamingfast/bstream"
	"github.com/streamingfast/bstream/blockstream"
	"github.com/streamingfast/bstream/forkable"
	"github.com/streamingfast/dstore"
	"github.com/streamingfast/kvdb"
	pbcodec "github.com/streamingfast/sf-ethereum/pb/sf/ethereum/codec/v1"
	"github.com/streamingfast/sf-ethereum/trxdb"
	"github.com/streamingfast/sf-ethereum/trxdb-loader/metrics"
	"github.com/streamingfast/shutter"
	"go.uber.org/zap"
)

type Job = func(blockNum uint64, blk *pbcodec.Block, fObj *forkable.ForkableObject) (err error)

type BigtableLoader struct {
	*shutter.Shutter
	processingJob             Job
	db                        trxdb.DBWriter
	batchSize                 uint64
	lastTickBlock             uint64
	lastTickTime              time.Time
	blocksStore               dstore.Store
	blockStreamAddr           string
	source                    bstream.Source
	endBlock                  uint64
	parallelFileDownloadCount int
	healthy                   bool
}

func NewBigtableLoader(
	blockStreamAddr string,
	blocksStore dstore.Store,
	batchSize uint64,
	db trxdb.DBWriter,
	parallelFileDownloadCount int,
) *BigtableLoader {
	loader := &BigtableLoader{
		blockStreamAddr:           blockStreamAddr,
		blocksStore:               blocksStore,
		Shutter:                   shutter.New(),
		db:                        db,
		batchSize:                 batchSize,
		parallelFileDownloadCount: parallelFileDownloadCount,
	}

	// By default, everything is assumed to be the full job, pipeline building overrides that
	loader.processingJob = loader.FullJob

	return loader
}

func (l *BigtableLoader) BuildPipelineLive(allowLiveOnEmptyTable bool) error {
	l.processingJob = l.FullJob

	startAtBlockOne := false
	libRef, err := l.db.GetLastWrittenIrreversibleBlockRef(context.Background())
	if err != nil {
		if err == kvdb.ErrNotFound && allowLiveOnEmptyTable {
			zlog.Info("forcing block start block 1")
			startAtBlockOne = true
		} else {
			return fmt.Errorf("failed getting latest written LIB: %w", err)
		}
	}

	sf := bstream.SourceFromRefFactory(func(startBlockRef bstream.BlockRef, h bstream.Handler) bstream.Source {
		var handler bstream.Handler
		var blockNum uint64
		var resolvedStartBlockRef bstream.BlockRef
		if startAtBlockOne {
			// We explicity want to start back from beginning, hence no gate at all
			zlog.Info("starting at block 1")
			handler = h
			blockNum = uint64(1)
		} else {
			// We start back from last written LIB, use a gate to start processing at the right place
			if startBlockRef.ID() == "" {
				resolvedStartBlockRef = libRef
			} else {
				resolvedStartBlockRef = startBlockRef
			}

			zlog.Info("setting exclusive block gate", zap.String("block_id", resolvedStartBlockRef.ID()))
			handler = bstream.NewBlockIDGate(resolvedStartBlockRef.ID(), bstream.GateExclusive, h, bstream.GateOptionWithLogger(zlog))
			blockNum = resolvedStartBlockRef.Num()
		}

		liveSourceFactory := bstream.SourceFactory(func(subHandler bstream.Handler) bstream.Source {
			src := blockstream.NewSource(
				context.Background(),
				l.blockStreamAddr,
				300,
				subHandler,
				blockstream.WithRequester("trxdb-loader"),
			)
			return src
		})
		fileSourceFactory := bstream.SourceFactory(func(subHandler bstream.Handler) bstream.Source {
			fs := bstream.NewFileSource(
				l.blocksStore,
				blockNum,
				l.parallelFileDownloadCount,
				nil,
				subHandler,
			)
			return fs
		})

		return bstream.NewJoiningSource(fileSourceFactory,
			liveSourceFactory,
			handler,
			bstream.JoiningSourceLogger(zlog),
			bstream.JoiningSourceTargetBlockID(startBlockRef.ID()),
			bstream.JoiningSourceTargetBlockNum(bstream.GetProtocolFirstStreamableBlock),
		)
	})

	forkableHandler := forkable.New(l,
		forkable.WithLogger(zlog),
		forkable.WithFilters(bstream.StepNew|bstream.StepIrreversible),
		forkable.EnsureAllBlocksTriggerLongestChain(),
	)

	es := bstream.NewEternalSource(sf, forkableHandler, bstream.EternalSourceWithLogger(zlog))
	l.source = es
	return nil
}

func (l *BigtableLoader) BuildPipelineBatch(startBlockNum uint64, startBlockResolver bstream.StartBlockResolver) {
	l.BuildPipelineJob(startBlockNum, startBlockResolver, l.FullJob)
}

func (l *BigtableLoader) BuildPipelinePatch(startBlockNum uint64, startBlockResolver bstream.StartBlockResolver) {
	l.BuildPipelineJob(startBlockNum, startBlockResolver, l.PatchJob)
}

func (l *BigtableLoader) BuildPipelineJob(startBlockNum uint64, startBlockResolver bstream.StartBlockResolver, job Job) {
	l.processingJob = job

	gate := bstream.NewBlockNumGate(startBlockNum, bstream.GateInclusive, l, bstream.GateOptionWithLogger(zlog))
	gate.MaxHoldOff = 1000

	forkableHandler := forkable.New(gate,
		forkable.WithLogger(zlog),
		forkable.WithFilters(bstream.StepNew|bstream.StepIrreversible),
	)

	resolvedStartBlockNum, _, err := startBlockResolver(context.Background(), startBlockNum)
	if err != nil {
		l.Shutdown(fmt.Errorf("unable to resolve start block: %w", err))
		return
	}

	fs := bstream.NewFileSource(
		l.blocksStore,
		resolvedStartBlockNum,
		l.parallelFileDownloadCount,
		nil,
		forkableHandler,
		bstream.FileSourceWithLogger(zlog),
	)
	l.source = fs
}

func (l *BigtableLoader) Launch() {
	l.source.OnTerminating(func(err error) {
		l.Shutdown(err)
	})
	l.source.OnTerminated(func(err error) {
		l.setUnhealthy()
	})
	l.OnTerminating(func(err error) {
		l.source.Shutdown(err)
	})
	l.source.Run()
}

// StopBeforeBlock indicates the stop block (exclusive), means that
// block num will not be inserted.
func (l *BigtableLoader) StopBeforeBlock(blockNum uint64) {
	l.endBlock = blockNum
}

func (l *BigtableLoader) setUnhealthy() {
	if l.healthy {
		l.healthy = false
	}
}

func (l *BigtableLoader) setHealthy() {
	if !l.healthy {
		l.healthy = true
	}
}

func (l *BigtableLoader) Healthy() bool {
	return l.healthy
}

// fullJob does all the database insertions needed to load the blockchain
// into our database.
func (l *BigtableLoader) FullJob(blockNum uint64, block *pbcodec.Block, fObj *forkable.ForkableObject) (err error) {
	blkTime := block.MustTime()

	switch fObj.Step() {
	case bstream.StepNew:
		l.ShowProgress(blockNum)
		l.setHealthy()

		defer metrics.HeadBlockTimeDrift.SetBlockTime(blkTime)
		defer metrics.HeadBlockNumber.SetUint64(blockNum)

		if blockNum == bstream.GetProtocolFirstStreamableBlock {
			genesisBlock := &pbcodec.Block{
				Number: bstream.GetProtocolGenesisBlock,
				Hash:   block.Header.ParentHash,
				Header: &pbcodec.BlockHeader{
					Hash:      block.Header.ParentHash,
					Number:    bstream.GetProtocolGenesisBlock,
					Timestamp: block.Header.Timestamp, // TODO FIXME when we can get the actual content of that genesis block
				},
			}
			if err := l.db.PutBlock(context.Background(), genesisBlock); err != nil {
				return fmt.Errorf("store genesis block: %w", err)
			}
			if err := l.db.UpdateNowIrreversibleBlock(context.Background(), genesisBlock); err != nil {
				return fmt.Errorf("set genesis block irreversible: %w", err)
			}
		}

		if err := l.db.PutBlock(context.Background(), block); err != nil {
			return fmt.Errorf("store block: %s", err)
		}
		return l.FlushIfNeeded(blockNum, blkTime)
	case bstream.StepIrreversible:
		if l.endBlock != 0 && blockNum >= l.endBlock && fObj.StepCount == fObj.StepIndex+1 {
			err := l.DoFlush(blockNum)
			if err != nil {
				l.Shutdown(err)
				return err
			}
			l.Shutdown(nil)
			return nil
		}

		// Handle only the first multi-block step Irreversible
		if fObj.StepIndex != 0 {
			return nil
		}

		if err := l.UpdateIrreversibleData(fObj.StepBlocks); err != nil {
			return err
		}

		err = l.FlushIfNeeded(blockNum, blkTime)
		if err != nil {
			zlog.Error("flushIfNeeded", zap.Error(err))
			return err
		}

		return nil

	default:
		return fmt.Errorf("unsupported forkable step %q", fObj.Step())
	}
}

func (l *BigtableLoader) ProcessBlock(blk *bstream.Block, obj interface{}) (err error) {
	if l.IsTerminating() {
		return nil
	}
	zlog.Debug("processing block", zap.Uint64("block_num", blk.Number), zap.String("block_id", blk.Id))
	return l.processingJob(blk.Num(), blk.ToNative().(*pbcodec.Block), obj.(*forkable.ForkableObject))
}

func (l *BigtableLoader) DoFlush(blockNum uint64) error {
	zlog.Debug("flushing block", zap.Uint64("block_num", blockNum))
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	err := l.db.Flush(ctx)
	if err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}
	return nil
}

func (l *BigtableLoader) FlushIfNeeded(blockNum uint64, blockTime time.Time) error {
	if blockNum%l.batchSize == 0 || time.Since(blockTime) < 25*time.Second {
		err := l.DoFlush(blockNum)
		if err != nil {
			return err
		}
		metrics.HeadBlockNumber.SetUint64(blockNum)
	}
	return nil
}

func (l *BigtableLoader) ShowProgress(blockNum uint64) {
	now := time.Now()
	if l.lastTickTime.Before(now.Add(-5 * time.Second)) {
		if !l.lastTickTime.IsZero() {
			zlog.Info("5sec AVG INSERT RATE",
				zap.Uint64("block_num", blockNum),
				zap.Uint64("last_tick_block", l.lastTickBlock),
				zap.Float64("block_sec", float64(blockNum-l.lastTickBlock)/float64(now.Sub(l.lastTickTime)/time.Second)),
			)
		}
		l.lastTickTime = now
		l.lastTickBlock = blockNum
	}
}

func (l *BigtableLoader) UpdateIrreversibleData(nowIrreversibleBlocks []*bstream.PreprocessedBlock) error {
	for _, blkObj := range nowIrreversibleBlocks {
		blk := blkObj.Block.ToNative().(*pbcodec.Block)

		if err := l.db.UpdateNowIrreversibleBlock(context.Background(), blk); err != nil {
			return err
		}
	}

	return nil
}

// patchDatabase is a "scratch" pad to define patch code that can be applied
// on an ad-hoc basis. The idea is to leave this function empty when no patch needs
// to be applied.
//
// When a patch is required, the suggested workflow is to develop the patch code in
// a side branch. When the code is ready, the "production" commit is tagged with the
// `patch-<tag>-<date>` where the tag is giving an overview of the patch and the date
// is the effective date (`<year>-<month>-<day>`): `patch-add-trx-meta-written-2019-06-30`.
// The branch is then deleted and the tag is pushed to the remote repository.
func (l *BigtableLoader) PatchJob(blockNum uint64, blk *pbcodec.Block, fObj *forkable.ForkableObject) (err error) {
	switch fObj.Step() {
	case bstream.StepNew:
		l.ShowProgress(blockNum)
		return l.FlushIfNeeded(blockNum, blk.MustTime())

	case bstream.StepIrreversible:
		if l.endBlock != 0 && blockNum >= l.endBlock && fObj.StepCount == fObj.StepIndex+1 {
			err := l.DoFlush(blockNum)
			if err != nil {
				return err
			}

			l.Shutdown(nil)
			return nil
		}
	}

	return nil
}
