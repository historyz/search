// Copyright 2019 dfuse Platform Inc.
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

package live

import (
	"context"
	"fmt"
	"math"

	"github.com/dfuse-io/bstream"
	"github.com/dfuse-io/bstream/forkable"
	"github.com/dfuse-io/derr"
	"github.com/dfuse-io/logging"
	v1 "github.com/dfuse-io/pbgo/dfuse/bstream/v1"
	"github.com/dfuse-io/search"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//
// ForwardLiveQuery
//

// type ForwardLiveQuery struct {
// 	archiveBlockRef bstream.BlockRef

// 	headBlockGetter func() *bstream.Block

// 	src bstream.Source

// 	ctx context.Context
// 	out chan<- *pb.SearchMatch
// }

// func (q *liveQuery) newForwardLiveQuery() *ForwardLiveQuery {
// 	return &ForwardLiveQuery{
// 		liveQuery:       q,
// 		headBlockGetter: hub.HeadBlock,
// 	}
// }

// NewForwardLiveQuery always starts consuming blocks the current archiveLIBID (which would be the lowest guaranteed starting point, and gives us an easy LIB ID for the forkable)
// the lowBlockNum should be higher than the archive LIB, it will start processing for realz at that point
// Be sure to call the releaseFunc if you don't call Query, so the source promise on block num can be released
func (q *liveQuery) runForwardQuery(firstLIBRef bstream.BlockRef) (err error) {
	// SUPER RESILIENT SEARCH CASE:
	//
	// If `qRange.cursorBlockID` is a STALLED block. If so, obtain
	// the Block IDs necessary to link back to the closest LIBID.
	///   * To do that, `blockmeta` will: check the passed block_id in Bigtable
	//      * If it's irreversible, stop the chain, otherwise, check the `previous_id`
	//        from that block, and fetch the block from bigtable again. Loop until
	//        we reach an irreversible block.

	// Prep the following:
	//  * Start a new FileSource at the LOWEST block_id returned by the `blockmeta` service, which IS irreversible.
	//  * Then, create a Forkable, on which we set the LIB to the one returned by `blockmeta`.
	//    * Set the Forkable to ENSURE it considers the `cursorBlockID` as a longest chain at least once (EnsureBlockIDFlows)
	//
	//  * We listen for `Undo` and `New`, consume the `Undo`
	//    elements only, and when we reach any block that isn't in
	//    the segment `blockmeta` gave us
	//    * When we see a `New` with a `previous_id` equal to the LIB ID
	//      returned by `blockmeta`, THEN we shutdown the source, as we're
	//      sure we implicitly passed that LIB. Handoff to archive.
	//
	// If there WAS a stalled block, the qRange's `low` boundary needs to be updated
	// to 1+ the LIB ID retrieved from `blockmeta`.

	src, err := q.setupForwardPipeline(firstLIBRef)
	if err != nil {
		return fmt.Errorf("fail to setup forward pipeline: %s", err)
	}
	src.Run()

	if err = src.Err(); err != nil {
		if err == context.Canceled {
			return derr.Status(codes.Canceled, "context canceled")
		}
		if st, ok := status.FromError(err); ok {
			if st.Code() == codes.Canceled {
				return nil
			}
		}
		// TODO: does caller handle that properly? or is it simply the end of the loop here and we
		// continue successfully in the caller's hands?
		if err != search.ErrEndOfRange {
			return err
		}
	}
	return nil
}

func (q *liveQuery) setupForwardPipeline(libRef bstream.BlockRef) (bstream.Source, error) {
	postForkGate := bstream.NewBlockNumGate(q.request.LowBlockNum, bstream.GateInclusive, bstream.HandlerFunc(q.forwardProcessBlock))

	options := []forkable.Option{
		forkable.WithInclusiveLIB(libRef),
	}

	if q.request.WithReversible {
		options = append(options, forkable.WithFilters(forkable.StepNew|forkable.StepUndo))
	} else {
		options = append(options, forkable.WithFilters(forkable.StepIrreversible))
	}

	forkableHandler := forkable.New(postForkGate, options...)

	next := bstream.Handler(forkableHandler)
	if q.request.LiveMarkerInterval > 0 {
		next = q.checkLiveMarkerFunc(next)
	}

	return q.sourceFromBlockNumFunc(libRef.Num(), next)
}

func (q *liveQuery) checkLiveMarkerFunc(h bstream.Handler) bstream.Handler {
	return bstream.HandlerFunc(func(blk *bstream.Block, obj interface{}) error {
		// TODO: make sure we handle the `withReversible == false` case, and `headBlockGetter()`
		// will actually return the IRR when `withReversible = false`.
		// TODO: test these cases
		if !q.liveMarkerReached {
			if q.isBlockOnHead(blk) {
				q.liveMarkerReached = true
			}
		}
		return h.ProcessBlock(blk, obj)
	})
}

func (q *liveQuery) isBlockOnHead(blk *bstream.Block) bool {
	_, _, _, irrID, _, headID := q.searchPeer.HeadBlockPointers()
	reachedID := irrID
	if q.request.WithReversible {
		reachedID = headID
	}
	return blk.ID() == reachedID
}

func (q *liveQuery) forwardProcessBlock(blk *bstream.Block, obj interface{}) error {
	if q.isAggregatorDone() {
		return derr.Status(codes.Canceled, "context canceled")
	}

	if blk.Num() > q.request.HighBlockNum {
		return search.ErrEndOfRange
	}

	fObj := obj.(*forkable.ForkableObject)
	idx := fObj.Obj.(*search.SingleIndex)

	matches, err := search.RunSingleIndexQuery(q.ctx, false, 0, math.MaxUint64, q.matchCollector, q.bleveQuery, idx.Index, func() {}, nil)
	if err != nil {
		if err == context.Canceled {
			return derr.Status(codes.Canceled, "context canceled")
		}
		logging.Logger(q.ctx, zlog).Error("error running single index query", zap.Error(err))
		return fmt.Errorf("failed running single-index query")
	}

	q.lastBlockRead = blk.Num()

	irrBlockNum := blk.LIBNum()
	if fObj.Step == forkable.StepIrreversible {
		irrBlockNum = blk.Num()
	}

	err = q.processMatches(matches, blk, irrBlockNum, fObj.Step)
	if err != nil {
		return err
	}

	if q.request.StopAtVirtualHead && q.isBlockOnHead(blk) {
		return search.ErrEndOfRange
	}

	blkNum := blk.Num()
	if blkNum == q.request.HighBlockNum {
		return search.ErrEndOfRange
	}
	if blkNum > q.request.HighBlockNum {
		zlog.Error(fmt.Sprintf("how come we could have skipped a block? high was: %d and we just processed %d but we didn't interrupt this process before!?", q.request.HighBlockNum, blkNum))
		return search.ErrEndOfRange
	}

	return nil
}

func (q *liveQuery) processMatches(matches []search.SearchMatch, blk *bstream.Block, irrBlockNum uint64, step forkable.StepType) error {
	for _, match := range matches {
		matchProto, err := liveSearchMatchToProto(blk, irrBlockNum, step == forkable.StepUndo, match)
		if err != nil {
			return fmt.Errorf("unable to create search match proto: %s", err)
		}

		select {
		case <-q.aggregatorDone:
			return nil
		case <-q.ctx.Done():
			return nil
		case q.incomingMatches <- matchProto:
		}
	}

	// send live marker
	if q.liveMarkerReached && step != forkable.StepUndo &&
		blk.Num() >= q.liveMarkerLastSentBlockNum+q.request.LiveMarkerInterval {

		var searchMatch search.SearchMatch
		if q.protocol == v1.Protocol_EOS {
			searchMatch = &search.EOSSearchMatch{}
		}

		matchProto, err := liveMarkerToProto(blk, irrBlockNum, searchMatch)
		if err != nil {
			return fmt.Errorf("unable to create search match proto: %s", err)
		}

		select {
		case <-q.aggregatorDone:
			return nil
		case <-q.ctx.Done():
			return nil
		case q.incomingMatches <- matchProto:
		}
		q.liveMarkerLastSentBlockNum = blk.Num()
	}
	return nil
}
