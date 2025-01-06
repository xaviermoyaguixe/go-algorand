// Copyright (C) 2019-2025 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

// Copyright (C) 2019-2024 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package ledger

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/algorand/go-deadlock"
	"github.com/golang/snappy"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merkletrie"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/logging/telemetryspec"
	"github.com/algorand/go-algorand/protocol"
)

const (
	// trieRebuildAccountChunkSize defines the number of accounts that would get read at a single chunk
	// before added to the trie during trie construction
	trieRebuildAccountChunkSize = 16384
	// trieRebuildCommitFrequency defines the number of accounts that would get added before we call evict to commit the changes and adjust the memory cache.
	trieRebuildCommitFrequency = 65536

	// CatchpointFileVersionV5 is the catchpoint file version that was used when the database schema was V0-V5.
	CatchpointFileVersionV5 = uint64(0200)
	// CatchpointFileVersionV6 is the catchpoint file version that is matching database schema since V6.
	// This version introduced accounts and resources separation. The first catchpoint
	// round of this version is >= `reenableCatchpointsRound`.
	CatchpointFileVersionV6 = uint64(0201)
	// CatchpointFileVersionV7 is the catchpoint file version that is matching database schema V10.
	// This version introduced state proof verification data and versioning for CatchpointLabel.
	CatchpointFileVersionV7 = uint64(0202)
	// CatchpointFileVersionV8 is the catchpoint file version that includes V6 and V7 data, as well
	// as historical onlineaccounts and onlineroundparamstail table data (added in DB version V7,
	// but until this version initialized with current round data, not 320 rounds of historical info).
	CatchpointFileVersionV8 = uint64(0203)

	// CatchpointContentFileName is a name of a file with catchpoint header info inside tar archive
	CatchpointContentFileName = "content.msgpack"
	// catchpointSPVerificationFileName is a name of a file with stateproof verification data
	catchpointSPVerificationFileName = "stateProofVerificationContext.msgpack"
	// catchpointBalancesFileNameTemplate is a template name of files with balances data
	catchpointBalancesFileNameTemplate = "balances.%d.msgpack"
	catchpointBalancesFileNamePrefix   = "balances."
	catchpointBalancesFileNameSuffix   = ".msgpack"
)

func catchpointStage1Encoder(w io.Writer) (io.WriteCloser, error) {
	return snappy.NewBufferedWriter(w), nil
}

type snappyReadCloser struct {
	*snappy.Reader
}

func (snappyReadCloser) Close() error { return nil }

func catchpointStage1Decoder(r io.Reader) (io.ReadCloser, error) {
	return snappyReadCloser{snappy.NewReader(r)}, nil
}

type catchpointTracker struct {
	// tmpDir is the path to the currently building catchpoint file
	tmpDir string
	// dbDirectory is the path to the finished/cold data of catchpoint
	dbDirectory string

	// catchpointInterval is the configured interval at which the catchpointTracker would generate catchpoint labels and catchpoint files.
	catchpointInterval uint64

	// catchpointFileHistoryLength defines how many catchpoint files we want to store back.
	// 0 means don't store any, -1 mean unlimited and positive number suggest the number of most recent catchpoint files.
	catchpointFileHistoryLength int

	// enableGeneratingCatchpointFiles determines whether catchpoints files should be generated by the trackers.
	enableGeneratingCatchpointFiles bool

	// log copied from ledger
	log logging.Logger

	// Connection to the database.
	dbs             trackerdb.Store
	catchpointStore trackerdb.CatchpointReaderWriter

	// The last catchpoint label that was written to the database. Should always align with what's in the database.
	// note that this is the last catchpoint *label* and not the catchpoint file.
	lastCatchpointLabel string

	// catchpointDataSlowWriting suggests to the accounts writer that it should finish
	// writing up the (first stage) catchpoint data file ASAP. When this channel is
	// closed, the accounts writer would try and complete the writing as soon as possible.
	// Otherwise, it would take its time and perform periodic sleeps between chunks
	// processing.
	catchpointDataSlowWriting chan struct{}

	// catchpointDataWriting helps to synchronize the (first stage) catchpoint data file
	// writing. When this atomic variable is 0, no writing is going on.
	// Any non-zero value indicates a catchpoint being written, or scheduled to be written.
	catchpointDataWriting atomic.Int32

	// The Trie tracking the current account balances. Always matches the balances that were
	// written to the database.
	balancesTrie *merkletrie.Trie

	// roundDigest stores the digest of the block for every round starting with dbRound+1 and every round after it.
	roundDigest []crypto.Digest

	// consensusVersion stores the consensus versions for every round starting with dbRound+1 and every round after it.
	consensusVersion []protocol.ConsensusVersion

	// reenableCatchpointsRound is a round where the EnableCatchpointsWithSPContexts feature was enabled via the consensus.
	// we avoid generating catchpoints before that round in order to ensure the network remain consistent in the catchpoint
	// label being produced. This variable could be "wrong" in two cases -
	// 1. It's zero, meaning that the EnableCatchpointsWithSPContexts has yet to be seen.
	// 2. It's non-zero meaning that it the given round is after the EnableCatchpointsWithSPContexts was enabled ( it might be exact round
	//    but that's only if newBlock was called with that round ), plus the lookback.
	reenableCatchpointsRound basics.Round

	// forceCatchpointFileWriting used for debugging purpose by bypassing the test against
	// reenableCatchpointsRound in isCatchpointRound(), so that we could generate
	// catchpoint files even before the protocol upgrade took place.
	forceCatchpointFileWriting bool

	// catchpointsMu protects roundDigest, reenableCatchpointsRound, cachedDBRound,
	// lastCatchpointLabel and balancesTrie.
	catchpointsMu deadlock.RWMutex

	// cachedDBRound is always exactly tracker DB round (and therefore, accountsRound()),
	// cached to use in lookup functions
	cachedDBRound basics.Round
}

// initialize initializes the catchpointTracker structure
func (ct *catchpointTracker) initialize(cfg config.Local, paths DirsAndPrefix) {
	// catchpoint uses the cold data directories, except for the temp file
	ct.dbDirectory = paths.CatchpointGenesisDir
	// the temp file uses the hot data directories
	ct.tmpDir = paths.HotGenesisDir

	if cfg.TracksCatchpoints() {
		ct.catchpointInterval = cfg.CatchpointInterval
	}
	ct.enableGeneratingCatchpointFiles = cfg.StoresCatchpoints()

	// Overwrite previous options if forceCatchpointFileGenerationTrackingMode
	if cfg.CatchpointTracking == forceCatchpointFileGenerationTrackingMode && cfg.CatchpointInterval > 0 {
		ct.catchpointInterval = cfg.CatchpointInterval
		ct.forceCatchpointFileWriting = true
		ct.enableGeneratingCatchpointFiles = true
	}

	ct.catchpointFileHistoryLength = cfg.CatchpointFileHistoryLength
	if cfg.CatchpointFileHistoryLength < -1 {
		ct.catchpointFileHistoryLength = -1
	}
}

// GetLastCatchpointLabel retrieves the last catchpoint label that was stored to the database.
func (ct *catchpointTracker) GetLastCatchpointLabel() string {
	ct.catchpointsMu.RLock()
	defer ct.catchpointsMu.RUnlock()
	return ct.lastCatchpointLabel
}

func (ct *catchpointTracker) getSPVerificationData() (encodedData []byte, spVerificationHash crypto.Digest, err error) {
	err = ct.dbs.Snapshot(func(ctx context.Context, tx trackerdb.SnapshotScope) error {
		rawData, dbErr := tx.MakeSpVerificationCtxReader().GetAllSPContexts(ctx)
		if dbErr != nil {
			return dbErr
		}

		wrappedData := catchpointStateProofVerificationContext{Data: rawData}
		spVerificationHash, encodedData = crypto.EncodeAndHash(wrappedData)
		return nil
	})
	if err != nil {
		return nil, crypto.Digest{}, err
	}
	return encodedData, spVerificationHash, nil
}

func (ct *catchpointTracker) finishFirstStage(ctx context.Context, dbRound basics.Round, blockProto protocol.ConsensusVersion, updatingBalancesDuration time.Duration) error {
	ct.log.Infof("finishing catchpoint's first stage dbRound: %d", dbRound)

	var totalAccounts, totalKVs, totalOnlineAccounts, totalOnlineRoundParams uint64
	var totalChunks uint64
	var biggestChunkLen uint64
	var spVerificationHash crypto.Digest
	var spVerificationEncodedData []byte
	var catchpointGenerationStats telemetryspec.CatchpointGenerationEventDetails
	var onlineAccountsHash, onlineRoundParamsHash crypto.Digest

	params := config.Consensus[blockProto]
	if params.EnableCatchpointsWithSPContexts {
		// Generate the SP Verification hash and encoded data. The hash is used in the label when tracking catchpoints,
		// and the encoded data for that hash will be added to the catchpoint file if catchpoint generation is enabled.
		var err error
		spVerificationEncodedData, spVerificationHash, err = ct.getSPVerificationData()
		if err != nil {
			return err
		}
	}
	if params.EnableCatchpointsWithOnlineAccounts {
		// Generate hashes of the onlineaccounts and onlineroundparams tables.
		err := ct.dbs.Snapshot(func(ctx context.Context, tx trackerdb.SnapshotScope) error {
			var dbErr error
			onlineAccountsHash, _, dbErr = calculateVerificationHash(ctx, tx.MakeOnlineAccountsIter, false)
			if dbErr != nil {
				return dbErr

			}

			onlineRoundParamsHash, _, dbErr = calculateVerificationHash(ctx, tx.MakeOnlineRoundParamsIter, false)
			if dbErr != nil {
				return dbErr
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	if ct.enableGeneratingCatchpointFiles {
		// Generate the catchpoint file. This is done inline so that it will
		// block any new accounts from being written. generateCatchpointData()
		// expects that the accounts data would not be modified in the
		// background during its execution.
		var err error

		catchpointGenerationStats.BalancesWriteTime = uint64(updatingBalancesDuration.Nanoseconds())
		totalAccounts, totalKVs, totalOnlineAccounts, totalOnlineRoundParams, totalChunks, biggestChunkLen, err = ct.generateCatchpointData(
			ctx, dbRound, &catchpointGenerationStats, spVerificationEncodedData)
		ct.catchpointDataWriting.Store(0)
		if err != nil {
			return err
		}
	}

	return ct.dbs.Transaction(func(ctx context.Context, tx trackerdb.TransactionScope) error {
		cw, err := tx.MakeCatchpointWriter()
		if err != nil {
			return err
		}

		err = ct.recordFirstStageInfo(ctx, tx, &catchpointGenerationStats, dbRound,
			totalAccounts, totalKVs, totalOnlineAccounts, totalOnlineRoundParams, totalChunks, biggestChunkLen,
			spVerificationHash, onlineAccountsHash, onlineRoundParamsHash)
		if err != nil {
			return err
		}

		// Clear the db record.
		return cw.WriteCatchpointStateUint64(ctx, trackerdb.CatchpointStateWritingFirstStageInfo, 0)
	})
}

// Possibly finish generating first stage catchpoint db record and data file after
// a crash.
func (ct *catchpointTracker) finishFirstStageAfterCrash(dbRound basics.Round, blockProto protocol.ConsensusVersion) error {
	v, err := ct.catchpointStore.ReadCatchpointStateUint64(
		context.Background(), trackerdb.CatchpointStateWritingFirstStageInfo)
	if err != nil {
		return err
	}
	if v == 0 {
		return nil
	}

	// First, delete the unfinished data file.
	relCatchpointDataFilePath := filepath.Join(trackerdb.CatchpointDirName, makeCatchpointDataFilePath(dbRound))
	err = trackerdb.RemoveSingleCatchpointFileFromDisk(ct.tmpDir, relCatchpointDataFilePath)
	if err != nil {
		return err
	}

	return ct.finishFirstStage(context.Background(), dbRound, blockProto, 0)
}

func (ct *catchpointTracker) finishCatchpointsAfterCrash(blockProto protocol.ConsensusVersion, catchpointLookback uint64) error {
	records, err := ct.catchpointStore.SelectUnfinishedCatchpoints(context.Background())
	if err != nil {
		return err
	}

	for _, record := range records {
		// First, delete the unfinished catchpoint file.
		relCatchpointFilePath := filepath.Join(trackerdb.CatchpointDirName, trackerdb.MakeCatchpointFilePath(basics.Round(record.Round)))
		err = trackerdb.RemoveSingleCatchpointFileFromDisk(ct.dbDirectory, relCatchpointFilePath)
		if err != nil {
			return err
		}

		err = ct.finishCatchpoint(
			context.Background(), record.Round, record.BlockHash, blockProto, catchpointLookback)
		if err != nil {
			return err
		}
	}

	return nil
}

func (ct *catchpointTracker) recoverFromCrash(dbRound basics.Round, blockProto protocol.ConsensusVersion) error {
	err := ct.finishFirstStageAfterCrash(dbRound, blockProto)
	if err != nil {
		return err
	}

	ctx := context.Background()

	catchpointLookback, err := ct.catchpointStore.ReadCatchpointStateUint64(
		ctx, trackerdb.CatchpointStateCatchpointLookback)
	if err != nil {
		return err
	}

	if catchpointLookback != 0 {
		err = ct.finishCatchpointsAfterCrash(blockProto, catchpointLookback)
		if err != nil {
			return err
		}

		if uint64(dbRound) >= catchpointLookback {
			err := ct.pruneFirstStageRecordsData(ctx, dbRound-basics.Round(catchpointLookback))
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// loadFromDisk loads the state of a tracker from persistent
// storage.  The ledger argument allows loadFromDisk to load
// blocks from the database, or access its own state.  The
// ledgerForTracker interface abstracts away the details of
// ledger internals so that individual trackers can be tested
// in isolation.
func (ct *catchpointTracker) loadFromDisk(l ledgerForTracker, dbRound basics.Round) (err error) {
	ct.log = l.trackerLog()
	ct.dbs = l.trackerDB()
	ct.catchpointStore, err = l.trackerDB().MakeCatchpointReaderWriter()
	if err != nil {
		return err
	}

	ct.catchpointsMu.Lock()
	ct.cachedDBRound = dbRound
	ct.roundDigest = nil
	ct.consensusVersion = nil
	ct.catchpointDataWriting.Store(0)
	// keep these channel closed if we're not generating catchpoint
	ct.catchpointDataSlowWriting = make(chan struct{}, 1)
	close(ct.catchpointDataSlowWriting)
	ct.catchpointsMu.Unlock()

	err = ct.dbs.Transaction(func(ctx context.Context, tx trackerdb.TransactionScope) error {
		return ct.initializeHashes(ctx, tx, dbRound)
	})
	if err != nil {
		return err
	}

	ct.lastCatchpointLabel, err = ct.catchpointStore.ReadCatchpointStateString(
		context.Background(), trackerdb.CatchpointStateLastCatchpoint)
	if err != nil {
		return
	}

	hdr, err := l.BlockHdr(dbRound)
	if err != nil {
		return
	}

	return ct.recoverFromCrash(dbRound, hdr.CurrentProtocol)
}

// newBlock informs the tracker of a new block from round
// rnd and a given ledgercore.StateDelta as produced by BlockEvaluator.
func (ct *catchpointTracker) newBlock(blk bookkeeping.Block, delta ledgercore.StateDelta) {
	ct.catchpointsMu.Lock()
	defer ct.catchpointsMu.Unlock()

	ct.roundDigest = append(ct.roundDigest, blk.Digest())
	ct.consensusVersion = append(ct.consensusVersion, blk.CurrentProtocol)

	if (config.Consensus[blk.CurrentProtocol].EnableCatchpointsWithSPContexts || ct.forceCatchpointFileWriting) && ct.reenableCatchpointsRound == 0 {
		catchpointLookback := config.Consensus[blk.CurrentProtocol].CatchpointLookback
		if catchpointLookback == 0 {
			catchpointLookback = config.Consensus[blk.CurrentProtocol].MaxBalLookback
		}
		ct.reenableCatchpointsRound = blk.BlockHeader.Round + basics.Round(catchpointLookback)
	}
}

// committedUpTo implements the ledgerTracker interface for catchpointTracker.
// The method informs the tracker that committedRound and all it's previous rounds have
// been committed to the block database. The method returns what is the oldest round
// number that can be removed from the blocks database as well as the lookback that this
// tracker maintains.
func (ct *catchpointTracker) committedUpTo(rnd basics.Round) (retRound, lookback basics.Round) {
	ct.catchpointsMu.RLock()
	defer ct.catchpointsMu.RUnlock()
	retRound = ct.cachedDBRound
	return retRound, basics.Round(0)
}

// Calculate whether we have intermediate first stage catchpoint rounds and the
// new offset.
func calculateFirstStageRounds(oldBase basics.Round, offset uint64, reenableCatchpointsRound basics.Round, catchpointInterval uint64, catchpointLookback uint64) (hasIntermediateFirstStageRound bool, hasMultipleIntermediateFirstStageRounds bool, newOffset uint64) {
	newOffset = offset

	if reenableCatchpointsRound == 0 {
		return
	}

	minFirstStageRound := oldBase + 1
	if (reenableCatchpointsRound > basics.Round(catchpointLookback)) &&
		(reenableCatchpointsRound-basics.Round(catchpointLookback) >
			minFirstStageRound) {
		minFirstStageRound =
			reenableCatchpointsRound - basics.Round(catchpointLookback)
	}

	// The smallest integer r >= minFirstStageRound such that
	// (r + catchpointLookback) % ct.catchpointInterval == 0.
	first := (int64(minFirstStageRound)+int64(catchpointLookback)+
		int64(catchpointInterval)-1)/
		int64(catchpointInterval)*int64(catchpointInterval) -
		int64(catchpointLookback)
	// The largest integer r <= dcr.oldBase + dcr.offset such that
	// (r + catchpointLookback) % ct.catchpointInterval == 0.
	last := (int64(oldBase)+int64(offset)+int64(catchpointLookback))/
		int64(catchpointInterval)*int64(catchpointInterval) - int64(catchpointLookback)

	if first <= last {
		hasIntermediateFirstStageRound = true
		// We skip earlier catchpoints if there is more than one to generate.
		newOffset = uint64(last) - uint64(oldBase)

		if first < last {
			hasMultipleIntermediateFirstStageRounds = true
		}
	}
	return
}

func (ct *catchpointTracker) produceCommittingTask(committedRound basics.Round, dbRound basics.Round, dcr *deferredCommitRange) *deferredCommitRange {
	if ct.catchpointInterval == 0 {
		return dcr
	}

	ct.catchpointsMu.Lock()
	reenableCatchpointsRound := ct.reenableCatchpointsRound
	ct.catchpointsMu.Unlock()

	// Check if we need to do the first stage of catchpoint generation.
	var hasIntermediateFirstStageRound bool
	var hasMultipleIntermediateFirstStageRounds bool
	hasIntermediateFirstStageRound, hasMultipleIntermediateFirstStageRounds, dcr.offset =
		calculateFirstStageRounds(
			dcr.oldBase, dcr.offset, reenableCatchpointsRound,
			ct.catchpointInterval, dcr.catchpointLookback)

	// if we're still writing the previous balances, we can't move forward yet.
	if ct.isWritingCatchpointDataFile() {
		// if we hit this path, it means that we're still writing a catchpoint.
		// see if the new delta range contains another catchpoint.
		if hasIntermediateFirstStageRound {
			// check if we're already attempting to perform fast-writing.
			select {
			case <-ct.catchpointDataSlowWriting:
				// yes, we're already doing fast-writing.
			default:
				// no, we're not yet doing fast writing, make it so.
				close(ct.catchpointDataSlowWriting)
			}
		}
		return nil
	}

	if hasIntermediateFirstStageRound {
		dcr.catchpointFirstStage = true

		if ct.enableGeneratingCatchpointFiles {
			ct.catchpointDataSlowWriting = make(chan struct{}, 1)
			if hasMultipleIntermediateFirstStageRounds {
				close(ct.catchpointDataSlowWriting)
			}
		}
	}

	dcr.enableGeneratingCatchpointFiles = ct.enableGeneratingCatchpointFiles

	rounds := ct.calculateCatchpointRounds(dcr)
	dcr.catchpointSecondStage = (len(rounds) > 0)

	return dcr
}

// prepareCommit, commitRound and postCommit are called when it is time to commit tracker's data.
// If an error returned the process is aborted.
func (ct *catchpointTracker) prepareCommit(dcc *deferredCommitContext) error {
	ct.catchpointsMu.RLock()
	defer ct.catchpointsMu.RUnlock()

	if ct.enableGeneratingCatchpointFiles && dcc.catchpointFirstStage {
		// store non-zero ( all ones ) into the catchpointWriting atomic variable to indicate that a catchpoint is being written
		ct.catchpointDataWriting.Store(int32(-1))
	}

	dcc.committedRoundDigests = make([]crypto.Digest, dcc.offset)
	copy(dcc.committedRoundDigests, ct.roundDigest[:dcc.offset])
	dcc.committedProtocolVersion = make([]protocol.ConsensusVersion, dcc.offset)
	copy(dcc.committedProtocolVersion, ct.consensusVersion[:dcc.offset])

	return nil
}

func (ct *catchpointTracker) commitRound(ctx context.Context, tx trackerdb.TransactionScope, dcc *deferredCommitContext) (err error) {
	treeTargetRound := basics.Round(0)
	offset := dcc.offset
	dbRound := dcc.oldBase

	defer func() {
		if err != nil && dcc.catchpointFirstStage && ct.enableGeneratingCatchpointFiles {
			ct.catchpointDataWriting.Store(0)
		}
	}()

	cw, err := tx.MakeCatchpointWriter()
	if err != nil {
		return err
	}
	aw, err := tx.MakeAccountsWriter()
	if err != nil {
		return err
	}

	if ct.catchpointEnabled() {
		var mc trackerdb.MerkleCommitter
		mc, err = tx.MakeMerkleCommitter(false)
		if err != nil {
			return
		}

		var trie *merkletrie.Trie
		ct.catchpointsMu.Lock()
		if ct.balancesTrie == nil {
			trie, err = merkletrie.MakeTrie(mc, trackerdb.TrieMemoryConfig)
			if err != nil {
				ct.log.Warnf("unable to create merkle trie during committedUpTo: %v", err)
				ct.catchpointsMu.Unlock()
				return err
			}
			ct.balancesTrie = trie
		} else {
			ct.balancesTrie.SetCommitter(mc)
		}
		ct.catchpointsMu.Unlock()
		treeTargetRound = dbRound + basics.Round(offset)
	}

	if dcc.updateStats {
		dcc.stats.MerkleTrieUpdateDuration = time.Duration(time.Now().UnixNano())
	}

	err = ct.accountsUpdateBalances(dcc.compactAccountDeltas, dcc.compactResourcesDeltas, dcc.compactKvDeltas, dcc.oldBase, dcc.newBase())
	if err != nil {
		return err
	}

	if dcc.updateStats {
		now := time.Duration(time.Now().UnixNano())
		dcc.stats.MerkleTrieUpdateDuration = now - dcc.stats.MerkleTrieUpdateDuration
	}

	err = aw.UpdateAccountsHashRound(ctx, treeTargetRound)
	if err != nil {
		return err
	}

	if dcc.catchpointFirstStage {
		err = cw.WriteCatchpointStateUint64(ctx, trackerdb.CatchpointStateWritingFirstStageInfo, 1)
		if err != nil {
			return err
		}
	}

	err = cw.WriteCatchpointStateUint64(ctx, trackerdb.CatchpointStateCatchpointLookback, dcc.catchpointLookback)
	if err != nil {
		return err
	}

	for _, round := range ct.calculateCatchpointRounds(&dcc.deferredCommitRange) {
		err = cw.InsertUnfinishedCatchpoint(ctx, round, dcc.committedRoundDigests[round-dcc.oldBase-1])
		if err != nil {
			return err
		}
	}

	return nil
}

func (ct *catchpointTracker) postCommit(ctx context.Context, dcc *deferredCommitContext) {
	ct.catchpointsMu.Lock()
	if ct.balancesTrie != nil {
		_, err := ct.balancesTrie.Evict(false)
		if err != nil {
			ct.log.Warnf("merkle trie failed to evict: %v", err)
		}
	}

	ct.roundDigest = ct.roundDigest[dcc.offset:]
	ct.consensusVersion = ct.consensusVersion[dcc.offset:]
	ct.cachedDBRound = dcc.newBase()
	ct.catchpointsMu.Unlock()

	dcc.updatingBalancesDuration = time.Since(dcc.flushTime)

	if dcc.updateStats {
		dcc.stats.MemoryUpdatesDuration = time.Duration(time.Now().UnixNano())
	}
}

func doRepackCatchpoint(ctx context.Context, header CatchpointFileHeader, biggestChunkLen uint64, in *tar.Reader, out *tar.Writer) error {
	bytes := protocol.Encode(&header)

	err := out.WriteHeader(&tar.Header{
		Name: CatchpointContentFileName,
		Mode: 0600,
		Size: int64(len(bytes)),
	})
	if err != nil {
		return err
	}

	_, err = out.Write(bytes)
	if err != nil {
		return err
	}

	// make buffer for re-use that can fit biggest chunk
	buf := make([]byte, biggestChunkLen)
	for {
		err := ctx.Err()
		if err != nil {
			return err
		}

		header, err := in.Next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		n, err := io.ReadAtLeast(in, buf, int(header.Size))
		if (err != nil) && (err != io.EOF) {
			return err
		}
		if int64(n) != header.Size { // should not happen
			return fmt.Errorf("read too many bytes from chunk %+v", header)
		}

		err = out.WriteHeader(header)
		if err != nil {
			return err
		}

		_, err = out.Write(buf[:header.Size])
		if err != nil {
			return err
		}
	}
}

// repackCatchpoint takes the header (that must be made "late" in order to have
// the latest blockhash) and the (snappy compressed) catchpoint data from
// dataPath and regurgitates it to look like catchpoints have always looked - a
// tar file with the header in the first "file" and the catchpoint data in file
// chunks, all compressed with gzip instead of snappy.
func repackCatchpoint(ctx context.Context, header CatchpointFileHeader, biggestChunkLen uint64, dataPath string, outPath string) error {
	// Initialize streams.
	fin, err := os.OpenFile(dataPath, os.O_RDONLY, 0666)
	if err != nil {
		return err
	}
	defer fin.Close()

	compressorIn, err := catchpointStage1Decoder(fin)
	if err != nil {
		return err
	}
	defer compressorIn.Close()

	tarIn := tar.NewReader(compressorIn)

	fout, err := os.OpenFile(outPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer fout.Close()

	gzipOut, err := gzip.NewWriterLevel(fout, gzip.BestSpeed)
	if err != nil {
		return err
	}
	defer gzipOut.Close()

	tarOut := tar.NewWriter(gzipOut)
	defer tarOut.Close()

	// Repack.
	err = doRepackCatchpoint(ctx, header, biggestChunkLen, tarIn, tarOut)
	if err != nil {
		return err
	}

	// Close streams.
	err = tarOut.Close()
	if err != nil {
		return err
	}

	err = gzipOut.Close()
	if err != nil {
		return err
	}

	err = fout.Close()
	if err != nil {
		return err
	}

	err = compressorIn.Close()
	if err != nil {
		return err
	}

	err = fin.Close()
	if err != nil {
		return err
	}

	return nil
}

// Create a catchpoint (a label and possibly a file with db record) and remove
// the unfinished catchpoint record.
func (ct *catchpointTracker) createCatchpoint(ctx context.Context, accountsRound basics.Round, round basics.Round, dataInfo trackerdb.CatchpointFirstStageInfo, blockHash crypto.Digest, blockProto protocol.ConsensusVersion) error {
	startTime := time.Now()
	var labelMaker ledgercore.CatchpointLabelMaker
	var version uint64
	params := config.Consensus[blockProto]
	if params.EnableCatchpointsWithOnlineAccounts {
		if !params.EnableCatchpointsWithSPContexts {
			return fmt.Errorf("invalid params for catchpoint file version v8: SP contexts not enabled")
		}
		labelMaker = ledgercore.MakeCatchpointLabelMakerCurrent(round, &blockHash, &dataInfo.TrieBalancesHash, dataInfo.Totals, &dataInfo.StateProofVerificationHash, &dataInfo.OnlineAccountsHash, &dataInfo.OnlineRoundParamsHash)
		version = CatchpointFileVersionV8
	} else if params.EnableCatchpointsWithSPContexts {
		labelMaker = ledgercore.MakeCatchpointLabelMakerV7(round, &blockHash, &dataInfo.TrieBalancesHash, dataInfo.Totals, &dataInfo.StateProofVerificationHash)
		version = CatchpointFileVersionV7
	} else {
		labelMaker = ledgercore.MakeCatchpointLabelMakerV6(round, &blockHash, &dataInfo.TrieBalancesHash, dataInfo.Totals)
		version = CatchpointFileVersionV6
	}
	label := ledgercore.MakeLabel(labelMaker)

	ct.log.Infof(
		"creating catchpoint round: %d accountsRound: %d label: %s",
		round, accountsRound, label)

	err := ct.catchpointStore.WriteCatchpointStateString(
		ctx, trackerdb.CatchpointStateLastCatchpoint, label)
	if err != nil {
		return err
	}

	ct.catchpointsMu.Lock()
	ct.lastCatchpointLabel = label
	ct.catchpointsMu.Unlock()

	if !ct.enableGeneratingCatchpointFiles {
		return nil
	}

	catchpointDataFilePath := filepath.Join(ct.tmpDir, trackerdb.CatchpointDirName)
	catchpointDataFilePath =
		filepath.Join(catchpointDataFilePath, makeCatchpointDataFilePath(accountsRound))

	// Check if the data file exists.
	_, err = os.Stat(catchpointDataFilePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	// Make a catchpoint file.
	header := CatchpointFileHeader{
		Version:                version,
		BalancesRound:          accountsRound,
		BlocksRound:            round,
		Totals:                 dataInfo.Totals,
		TotalAccounts:          dataInfo.TotalAccounts,
		TotalKVs:               dataInfo.TotalKVs,
		TotalOnlineAccounts:    dataInfo.TotalOnlineAccounts,
		TotalOnlineRoundParams: dataInfo.TotalOnlineRoundParams,
		TotalChunks:            dataInfo.TotalChunks,
		Catchpoint:             label,
		BlockHeaderDigest:      blockHash,
	}

	relCatchpointFilePath := filepath.Join(trackerdb.CatchpointDirName, trackerdb.MakeCatchpointFilePath(round))
	absCatchpointFilePath := filepath.Join(ct.dbDirectory, relCatchpointFilePath)

	err = os.MkdirAll(filepath.Dir(absCatchpointFilePath), 0700)
	if err != nil {
		return err
	}

	err = repackCatchpoint(ctx, header, dataInfo.BiggestChunkLen, catchpointDataFilePath, absCatchpointFilePath)
	if err != nil {
		return err
	}

	fileInfo, err := os.Stat(absCatchpointFilePath)
	if err != nil {
		return err
	}

	err = ct.dbs.Transaction(func(ctx context.Context, tx trackerdb.TransactionScope) (err error) {
		crw, err := tx.MakeCatchpointReaderWriter()
		if err != nil {
			return err
		}

		err = ct.recordCatchpointFile(ctx, crw, round, relCatchpointFilePath, fileInfo.Size())
		if err != nil {
			return err
		}
		return crw.DeleteUnfinishedCatchpoint(ctx, round)
	})
	if err != nil {
		return err
	}

	ct.log.With("accountsRound", accountsRound).
		With("writingDuration", uint64(time.Since(startTime).Nanoseconds())).
		With("accountsCount", dataInfo.TotalAccounts).
		With("kvsCount", dataInfo.TotalKVs).
		With("onlineAccountsCount", dataInfo.TotalOnlineAccounts).
		With("onlineRoundParamsCount", dataInfo.TotalOnlineRoundParams).
		With("fileSize", fileInfo.Size()).
		With("filepath", relCatchpointFilePath).
		With("catchpointLabel", label).
		Infof("Catchpoint file was created")

	return nil
}

// Try create a catchpoint (a label and possibly a file with db record) and remove
// the unfinished catchpoint record.
func (ct *catchpointTracker) finishCatchpoint(ctx context.Context, round basics.Round, blockHash crypto.Digest, blockProto protocol.ConsensusVersion, catchpointLookback uint64) error {
	accountsRound := round - basics.Round(catchpointLookback)

	ct.log.Infof("finishing catchpoint round: %d accountsRound: %d", round, accountsRound)

	dataInfo, exists, err := ct.catchpointStore.SelectCatchpointFirstStageInfo(ctx, accountsRound)
	if err != nil {
		return err
	}

	if !exists {
		return ct.catchpointStore.DeleteUnfinishedCatchpoint(ctx, round)
	}
	return ct.createCatchpoint(ctx, accountsRound, round, dataInfo, blockHash, blockProto)
}

// Calculate catchpoint round numbers in [min, max]. `catchpointInterval` must be
// non-zero.
func calculateCatchpointRounds(min basics.Round, max basics.Round, catchpointInterval uint64) []basics.Round {

	// The smallest integer i such that i * ct.catchpointInterval >= min.
	l := (uint64(min) + catchpointInterval - 1) / catchpointInterval
	// The largest integer i such that i * ct.catchpointInterval <= max.
	r := uint64(max) / catchpointInterval

	// handle situations when max - min < catchpointInterval,
	// for example min=11, max=19, catchpointInterval = 10
	if l > r {
		return nil
	}

	res := make([]basics.Round, 0, r-l+1)
	for i := l; i <= r; i++ {
		round := basics.Round(i * catchpointInterval)
		res = append(res, round)
	}

	return res
}

func (ct *catchpointTracker) calculateCatchpointRounds(dcc *deferredCommitRange) []basics.Round {
	if ct.catchpointInterval == 0 {
		return nil
	}

	min := dcc.oldBase + 1
	if dcc.catchpointLookback+1 > uint64(min) {
		min = basics.Round(dcc.catchpointLookback) + 1
	}
	max := dcc.oldBase + basics.Round(dcc.offset)
	return calculateCatchpointRounds(min, max, ct.catchpointInterval)
}

// Delete old first stage catchpoint records and data files.
func (ct *catchpointTracker) pruneFirstStageRecordsData(ctx context.Context, maxRoundToDelete basics.Round) error {
	rounds, err := ct.catchpointStore.SelectOldCatchpointFirstStageInfoRounds(ctx, maxRoundToDelete)
	if err != nil {
		return err
	}

	for _, round := range rounds {
		relCatchpointDataFilePath :=
			filepath.Join(trackerdb.CatchpointDirName, makeCatchpointDataFilePath(round))
		err = trackerdb.RemoveSingleCatchpointFileFromDisk(ct.tmpDir, relCatchpointDataFilePath)
		if err != nil {
			return err
		}
	}

	return ct.catchpointStore.DeleteOldCatchpointFirstStageInfo(ctx, maxRoundToDelete)
}

func (ct *catchpointTracker) postCommitUnlocked(ctx context.Context, dcc *deferredCommitContext) {
	if dcc.catchpointFirstStage {
		round := dcc.newBase()
		blockProto := dcc.committedProtocolVersion[round-dcc.oldBase-1]
		err := ct.finishFirstStage(ctx, round, blockProto, dcc.updatingBalancesDuration)
		if err != nil {
			ct.log.Warnf(
				"error finishing catchpoint's first stage dcc.newBase: %d err: %v",
				dcc.newBase(), err)
		}
	}

	// Generate catchpoints for rounds in (dcc.oldBase, dcc.newBase].
	for _, round := range ct.calculateCatchpointRounds(&dcc.deferredCommitRange) {
		blockHash := dcc.committedRoundDigests[round-dcc.oldBase-1]
		blockProto := dcc.committedProtocolVersion[round-dcc.oldBase-1]
		err := ct.finishCatchpoint(
			ctx, round, blockHash, blockProto, dcc.catchpointLookback)
		if err != nil {
			ct.log.Warnf("error creating catchpoint round: %d err: %v", round, err)
		}
	}

	// Prune first stage catchpoint records from the database.
	if uint64(dcc.newBase()) >= dcc.catchpointLookback {
		err := ct.pruneFirstStageRecordsData(
			ctx, dcc.newBase()-basics.Round(dcc.catchpointLookback))
		if err != nil {
			ct.log.Warnf(
				"error pruning first stage records and data dcc.newBase: %d err: %v",
				dcc.newBase(), err)
		}
	}
}

// when the deferred commit is found to be out of order, cancel writing
func (ct *catchpointTracker) handleUnorderedCommit(dcc *deferredCommitContext) {
	ct.cancelWrite(dcc)
}

// if an error is encountered during commit preparation, cancel writing
func (ct *catchpointTracker) handlePrepareCommitError(dcc *deferredCommitContext) {
	ct.cancelWrite(dcc)
}

// if an error is encountered between retries, clear the balancesTrie to clear in-memory changes made in commitRound().
func (ct *catchpointTracker) clearCommitRoundRetry(ctx context.Context, dcc *deferredCommitContext) {
	ct.log.Infof("rolling back failed commitRound for oldBase %d offset %d, clearing balancesTrie", dcc.oldBase, dcc.offset)
	ct.catchpointsMu.Lock()
	ct.balancesTrie = nil // balancesTrie will be re-created in the next call to commitRound
	ct.catchpointsMu.Unlock()
}

// if an error is encountered during commit, cancel writing and clear the balances trie
func (ct *catchpointTracker) handleCommitError(dcc *deferredCommitContext) {
	// in cases where the commitRound fails, it is not certain that the merkle trie is in a clean state, and should be cleared.
	// Specifically, modifications to the trie happen through accountsUpdateBalances,
	// which happens before commit to disk. Errors in this tracker, subsequent trackers, or the commit to disk may cause the trie cache to be incorrect,
	// affecting the perceived root on subsequent rounds
	ct.catchpointsMu.Lock()
	ct.balancesTrie = nil
	ct.catchpointsMu.Unlock()
	ct.cancelWrite(dcc)
}

func (ct *catchpointTracker) cancelWrite(dcc *deferredCommitContext) {
	// if the node is configured to generate catchpoint files, we might need to update the catchpointWriting variable.
	if ct.enableGeneratingCatchpointFiles {
		// determine if this was a catchpoint round
		if dcc.catchpointFirstStage {
			// it was a catchpoint round, so update the catchpointWriting to indicate that we're done.
			ct.catchpointDataWriting.Store(0)
		}
	}
}

// close terminates the tracker, reclaiming any resources
// like open database connections or goroutines.  close may
// be called even if loadFromDisk() is not called or does
// not succeed.
func (ct *catchpointTracker) close() {
}

// accountsUpdateBalances applies the given compactAccountDeltas to the merkle trie
func (ct *catchpointTracker) accountsUpdateBalances(accountsDeltas compactAccountDeltas, resourcesDeltas compactResourcesDeltas, kvDeltas map[string]modifiedKvValue, oldBase basics.Round, newBase basics.Round) error {
	if !ct.catchpointEnabled() {
		return nil
	}
	accumulatedChanges := 0

	for i := 0; i < accountsDeltas.len(); i++ {
		delta := accountsDeltas.getByIdx(i)
		if !delta.oldAcct.AccountData.IsEmpty() {
			deleteHash := trackerdb.AccountHashBuilderV6(delta.address, &delta.oldAcct.AccountData, protocol.Encode(&delta.oldAcct.AccountData))
			deleted, err := ct.balancesTrie.Delete(deleteHash)
			if err != nil {
				return fmt.Errorf("failed to delete hash '%s' from merkle trie for account %v: %w", hex.EncodeToString(deleteHash), delta.address, err)
			}
			if !deleted {
				ct.log.Errorf("failed to delete hash '%s' from merkle trie for account %v", hex.EncodeToString(deleteHash), delta.address)
			} else {
				accumulatedChanges++
			}
		}

		if !delta.newAcct.IsEmpty() {
			addHash := trackerdb.AccountHashBuilderV6(delta.address, &delta.newAcct, protocol.Encode(&delta.newAcct))
			added, err := ct.balancesTrie.Add(addHash)
			if err != nil {
				return fmt.Errorf("attempted to add duplicate hash '%s' to merkle trie for account %v: %w", hex.EncodeToString(addHash), delta.address, err)
			}
			if !added {
				ct.log.Errorf("attempted to add duplicate hash '%s' to merkle trie for account %v", hex.EncodeToString(addHash), delta.address)
			} else {
				accumulatedChanges++
			}
		}
	}

	for i := 0; i < resourcesDeltas.len(); i++ {
		resDelta := resourcesDeltas.getByIdx(i)
		addr := resDelta.address
		if !resDelta.oldResource.Data.IsEmpty() {
			deleteHash, err := trackerdb.ResourcesHashBuilderV6(&resDelta.oldResource.Data, addr, resDelta.oldResource.Aidx, resDelta.oldResource.Data.UpdateRound, protocol.Encode(&resDelta.oldResource.Data))
			if err != nil {
				return err
			}
			deleted, err := ct.balancesTrie.Delete(deleteHash)
			if err != nil {
				return fmt.Errorf("failed to delete resource hash '%s' from merkle trie for account %v: %w", hex.EncodeToString(deleteHash), addr, err)
			}
			if !deleted {
				ct.log.Errorf("failed to delete resource hash '%s' from merkle trie for account %v", hex.EncodeToString(deleteHash), addr)
			} else {
				accumulatedChanges++
			}
		}

		if !resDelta.newResource.IsEmpty() {
			addHash, err := trackerdb.ResourcesHashBuilderV6(&resDelta.newResource, addr, resDelta.oldResource.Aidx, resDelta.newResource.UpdateRound, protocol.Encode(&resDelta.newResource))
			if err != nil {
				return err
			}
			added, err := ct.balancesTrie.Add(addHash)
			if err != nil {
				return fmt.Errorf("attempted to add duplicate resource hash '%s' to merkle trie for account %v: %w", hex.EncodeToString(addHash), addr, err)
			}
			if !added {
				ct.log.Errorf("attempted to add duplicate resource hash '%s' to merkle trie for account %v", hex.EncodeToString(addHash), addr)
			} else {
				accumulatedChanges++
			}
		}
	}

	for key, mv := range kvDeltas {
		if mv.oldData == nil && mv.data == nil { // Came and went within the delta span
			continue
		}
		if mv.oldData != nil {
			// reminder: check mv.data for nil here, b/c bytes.Equal conflates nil and "".
			if mv.data != nil && bytes.Equal(mv.oldData, mv.data) {
				continue // changed back within the delta span
			}
			deleteHash := trackerdb.KvHashBuilderV6(key, mv.oldData)
			deleted, err := ct.balancesTrie.Delete(deleteHash)
			if err != nil {
				return fmt.Errorf("failed to delete kv hash '%s' from merkle trie for key %v: %w", hex.EncodeToString(deleteHash), key, err)
			}
			if !deleted {
				ct.log.Errorf("failed to delete kv hash '%s' from merkle trie for key %v", hex.EncodeToString(deleteHash), key)
			} else {
				accumulatedChanges++
			}
		}

		if mv.data != nil {
			addHash := trackerdb.KvHashBuilderV6(key, mv.data)
			added, err := ct.balancesTrie.Add(addHash)
			if err != nil {
				return fmt.Errorf("attempted to add duplicate kv hash '%s' from merkle trie for key %v: %w", hex.EncodeToString(addHash), key, err)
			}
			if !added {
				ct.log.Errorf("attempted to add duplicate kv hash '%s' from merkle trie for key %v", hex.EncodeToString(addHash), key)
			} else {
				accumulatedChanges++
			}
		}
	}

	// write it all to disk.
	var cstats merkletrie.CommitStats
	var commitErr error
	if accumulatedChanges > 0 {
		cstats, commitErr = ct.balancesTrie.Commit()
	}

	if ct.log.GetTelemetryEnabled() {
		root, rootErr := ct.balancesTrie.RootHash()
		if rootErr != nil {
			// log rootErr if failed to fetch for reporting in telemetry, then return whether Commit() succeeded or not
			ct.log.Errorf("accountsUpdateBalances: error retrieving balances trie root: %v", rootErr)
			return commitErr
		}
		ct.log.EventWithDetails(telemetryspec.Accounts, telemetryspec.CatchpointRootUpdateEvent, telemetryspec.CatchpointRootUpdateEventDetails{
			Root:                        root.String(),
			OldBase:                     uint64(oldBase),
			NewBase:                     uint64(newBase),
			NewPageCount:                cstats.NewPageCount,
			NewNodeCount:                cstats.NewNodeCount,
			UpdatedPageCount:            cstats.UpdatedPageCount,
			UpdatedNodeCount:            cstats.UpdatedNodeCount,
			DeletedPageCount:            cstats.DeletedPageCount,
			FanoutReallocatedNodeCount:  cstats.FanoutReallocatedNodeCount,
			PackingReallocatedNodeCount: cstats.PackingReallocatedNodeCount,
			LoadedPages:                 cstats.LoadedPages,
		})

	}
	return commitErr
}

// isWritingCatchpointDataFile returns true iff a (first stage) catchpoint data file
// is being generated.
func (ct *catchpointTracker) isWritingCatchpointDataFile() bool {
	return ct.catchpointDataWriting.Load() != 0
}

// Generates a (first stage) catchpoint data file.
// The file is built in the following order:
//   - Catchpoint file header (named content.msgpack). The header is generated and appended to the file at the end of the
//     second stage of catchpoint generation.
//   - State proof verification data chunk (named stateProofVerificationContext.msgpack).
//   - Balance and KV chunk (named balances.x.msgpack).
//     ...
//   - Balance and KV chunk (named balances.x.msgpack).
func (ct *catchpointTracker) generateCatchpointData(ctx context.Context, accountsRound basics.Round, catchpointGenerationStats *telemetryspec.CatchpointGenerationEventDetails, encodedSPData []byte) (totalAccounts, totalKVs, totalOnlineAccounts, totalOnlineRoundParams, totalChunks, biggestChunkLen uint64, err error) {
	ct.log.Debugf("catchpointTracker.generateCatchpointData() writing catchpoint accounts for round %d", accountsRound)

	startTime := time.Now()

	catchpointDataFilePath := filepath.Join(ct.tmpDir, trackerdb.CatchpointDirName)
	catchpointDataFilePath = filepath.Join(catchpointDataFilePath, makeCatchpointDataFilePath(accountsRound))

	more := true
	const shortChunkExecutionDuration = 50 * time.Millisecond
	const longChunkExecutionDuration = 1 * time.Second
	var chunkExecutionDuration time.Duration
	select {
	case <-ct.catchpointDataSlowWriting:
		chunkExecutionDuration = longChunkExecutionDuration
	default:
		chunkExecutionDuration = shortChunkExecutionDuration
	}

	var catchpointWriter *catchpointFileWriter

	start := time.Now()
	ledgerGeneratecatchpointCount.Inc(nil)
	err = ct.dbs.SnapshotContext(ctx, func(dbCtx context.Context, tx trackerdb.SnapshotScope) (err error) {
		catchpointWriter, err = makeCatchpointFileWriter(dbCtx, catchpointDataFilePath, tx, ResourcesPerCatchpointFileChunk)
		if err != nil {
			return
		}

		// do not write encodedSPData if not provided,
		// this is an indication the older catchpoint file is being generated.
		if encodedSPData != nil {
			err = catchpointWriter.FileWriteSPVerificationContext(encodedSPData)
			if err != nil {
				return
			}
		}

		for more {
			stepCtx, stepCancelFunction := context.WithTimeout(dbCtx, chunkExecutionDuration)
			writeStepStartTime := time.Now()
			more, err = catchpointWriter.FileWriteStep(stepCtx)
			// accumulate the actual time we've spent writing in this step.
			catchpointGenerationStats.CPUTime += uint64(time.Since(writeStepStartTime).Nanoseconds())
			stepCancelFunction()
			if more && err == nil {
				// we just wrote some data, but there is more to be written.
				// go to sleep for while.
				// before going to sleep, extend the transaction timeout so that we won't get warnings:
				_, err0 := tx.ResetTransactionWarnDeadline(dbCtx, time.Now().Add(1*time.Second))
				if err0 != nil {
					ct.log.Warnf("catchpointTracker: generateCatchpoint: failed to reset transaction warn deadline : %v", err0)
				}
				select {
				case <-time.After(100 * time.Millisecond):
					// increase the time slot allocated for writing the catchpoint, but stop when we get to the longChunkExecutionDuration limit.
					// this would allow the catchpoint writing speed to ramp up while still leaving some cpu available.
					chunkExecutionDuration *= 2
					if chunkExecutionDuration > longChunkExecutionDuration {
						chunkExecutionDuration = longChunkExecutionDuration
					}
				case <-dbCtx.Done():
					//retryCatchpointCreation = true
					err2 := catchpointWriter.Abort()
					if err2 != nil {
						return fmt.Errorf("error removing catchpoint file : %v", err2)
					}
					return nil
				case <-ct.catchpointDataSlowWriting:
					chunkExecutionDuration = longChunkExecutionDuration
				}
			}
			if err != nil {
				err = fmt.Errorf(
					"unable to create catchpoint data file for round %d: %v",
					accountsRound, err)
				err2 := catchpointWriter.Abort()
				if err2 != nil {
					ct.log.Warnf("catchpointTracker.generateCatchpointData() error removing catchpoint file : %v", err2)
				}
				return
			}
		}
		return
	})
	ledgerGeneratecatchpointMicros.AddMicrosecondsSince(start, nil)
	if err != nil {
		ct.log.Warnf("catchpointTracker.generateCatchpointData() %v", err)
		return 0, 0, 0, 0, 0, 0, err
	}

	catchpointGenerationStats.FileSize = uint64(catchpointWriter.writtenBytes)
	catchpointGenerationStats.WritingDuration = uint64(time.Since(startTime).Nanoseconds())
	catchpointGenerationStats.AccountsCount = catchpointWriter.totalAccounts
	catchpointGenerationStats.KVsCount = catchpointWriter.totalKVs
	catchpointGenerationStats.OnlineAccountsCount = catchpointWriter.totalOnlineAccounts
	catchpointGenerationStats.OnlineRoundParamsCount = catchpointWriter.totalOnlineRoundParams
	catchpointGenerationStats.AccountsRound = uint64(accountsRound)

	return catchpointWriter.totalAccounts, catchpointWriter.totalKVs, catchpointWriter.totalOnlineAccounts, catchpointWriter.totalOnlineRoundParams, catchpointWriter.chunkNum, catchpointWriter.biggestChunkLen, nil
}

func (ct *catchpointTracker) recordFirstStageInfo(ctx context.Context, tx trackerdb.TransactionScope,
	catchpointGenerationStats *telemetryspec.CatchpointGenerationEventDetails,
	accountsRound basics.Round,
	totalAccounts, totalKVs, totalOnlineAccounts, totalOnlineRoundParams, totalChunks, biggestChunkLen uint64,
	stateProofVerificationHash, onlineAccountsVerificationHash, onlineRoundParamsVerificationHash crypto.Digest) error {
	ar, err := tx.MakeAccountsReader()
	if err != nil {
		return err
	}

	accountTotals, err := ar.AccountsTotals(ctx, false)
	if err != nil {
		return err
	}

	mc, err := tx.MakeMerkleCommitter(false)
	if err != nil {
		return err
	}
	ct.catchpointsMu.Lock()
	if ct.balancesTrie == nil {
		trie, trieErr := merkletrie.MakeTrie(mc, trackerdb.TrieMemoryConfig)
		if trieErr != nil {
			ct.catchpointsMu.Unlock()
			return trieErr
		}
		ct.balancesTrie = trie
	} else {
		ct.balancesTrie.SetCommitter(mc)
	}

	trieBalancesHash, err := ct.balancesTrie.RootHash()
	if err != nil {
		ct.catchpointsMu.Unlock()
		return err
	}
	ct.catchpointsMu.Unlock()

	cw, err := tx.MakeCatchpointWriter()
	if err != nil {
		return err
	}

	info := trackerdb.CatchpointFirstStageInfo{
		Totals:                     accountTotals,
		TotalAccounts:              totalAccounts,
		TotalKVs:                   totalKVs,
		TotalOnlineAccounts:        totalOnlineAccounts,
		TotalOnlineRoundParams:     totalOnlineRoundParams,
		TotalChunks:                totalChunks,
		BiggestChunkLen:            biggestChunkLen,
		TrieBalancesHash:           trieBalancesHash,
		StateProofVerificationHash: stateProofVerificationHash,
		OnlineAccountsHash:         onlineAccountsVerificationHash,
		OnlineRoundParamsHash:      onlineRoundParamsVerificationHash,
	}

	err = cw.InsertOrReplaceCatchpointFirstStageInfo(ctx, accountsRound, &info)
	if err != nil {
		return err
	}

	catchpointGenerationStats.MerkleTrieRootHash = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(trieBalancesHash[:])
	catchpointGenerationStats.SPVerificationCtxsHash = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(stateProofVerificationHash[:])
	ct.log.EventWithDetails(telemetryspec.Accounts, telemetryspec.CatchpointGenerationEvent, catchpointGenerationStats)
	ct.log.With("accountsRound", catchpointGenerationStats.AccountsRound).
		With("writingDuration", catchpointGenerationStats.WritingDuration).
		With("CPUTime", catchpointGenerationStats.CPUTime).
		With("balancesWriteTime", catchpointGenerationStats.BalancesWriteTime).
		With("accountsCount", catchpointGenerationStats.AccountsCount).
		With("kvsCount", catchpointGenerationStats.KVsCount).
		With("fileSize", catchpointGenerationStats.FileSize).
		With("MerkleTrieRootHash", catchpointGenerationStats.MerkleTrieRootHash).
		With("SPVerificationCtxsHash", catchpointGenerationStats.SPVerificationCtxsHash).
		Infof("Catchpoint data file was generated")
	return nil
}

func makeCatchpointDataFilePath(accountsRound basics.Round) string {
	return strconv.FormatInt(int64(accountsRound), 10) + ".data"
}

// recordCatchpointFile stores the provided fileName as the stored catchpoint for the given round.
// after a successful insert operation to the database, it would delete up to 2 old entries, as needed.
// deleting 2 entries while inserting single entry allow us to adjust the size of the backing storage and have the
// database and storage realign.
func (ct *catchpointTracker) recordCatchpointFile(ctx context.Context, crw trackerdb.CatchpointReaderWriter, round basics.Round, relCatchpointFilePath string, fileSize int64) (err error) {
	if ct.catchpointFileHistoryLength != 0 {
		err = crw.StoreCatchpoint(ctx, round, relCatchpointFilePath, "", fileSize)
		if err != nil {
			ct.log.Warnf("catchpointTracker.recordCatchpointFile() unable to save catchpoint: %v", err)
			return
		}
	} else {
		err = trackerdb.RemoveSingleCatchpointFileFromDisk(ct.dbDirectory, relCatchpointFilePath)
		if err != nil {
			ct.log.Warnf("catchpointTracker.recordCatchpointFile() unable to remove file (%s): %v", relCatchpointFilePath, err)
			return
		}
	}
	if ct.catchpointFileHistoryLength == -1 {
		return
	}
	var filesToDelete map[basics.Round]string
	filesToDelete, err = crw.GetOldestCatchpointFiles(ctx, 2, ct.catchpointFileHistoryLength)
	if err != nil {
		return fmt.Errorf("unable to delete catchpoint file, getOldestCatchpointFiles failed : %v", err)
	}
	for round, fileToDelete := range filesToDelete {
		err = trackerdb.RemoveSingleCatchpointFileFromDisk(ct.dbDirectory, fileToDelete)
		if err != nil {
			return err
		}
		err = crw.StoreCatchpoint(ctx, round, "", "", 0)
		if err != nil {
			return fmt.Errorf("unable to delete old catchpoint entry '%s' : %v", fileToDelete, err)
		}
	}
	return
}

// GetCatchpointStream returns a ReadCloseSizer to the catchpoint file associated with the provided round
func (ct *catchpointTracker) GetCatchpointStream(round basics.Round) (ReadCloseSizer, error) {
	dbFileName := ""
	fileSize := int64(0)
	start := time.Now()
	ledgerGetcatchpointCount.Inc(nil)
	// TODO: we need to generalize this, check @cce PoC PR, he has something
	//       somewhat broken for some KVs..
	err := ct.dbs.Snapshot(func(ctx context.Context, tx trackerdb.SnapshotScope) (err error) {
		cr, err := tx.MakeCatchpointReader()
		if err != nil {
			return err
		}

		dbFileName, _, fileSize, err = cr.GetCatchpoint(ctx, round)
		return
	})
	ledgerGetcatchpointMicros.AddMicrosecondsSince(start, nil)
	if err != nil && err != sql.ErrNoRows {
		// we had some sql error.
		return nil, fmt.Errorf("catchpointTracker.GetCatchpointStream() unable to lookup catchpoint %d: %v", round, err)
	}
	if dbFileName != "" {
		catchpointPath := filepath.Join(ct.dbDirectory, dbFileName)
		file, openErr := os.OpenFile(catchpointPath, os.O_RDONLY, 0666)
		if openErr == nil && file != nil {
			return &readCloseSizer{ReadCloser: file, size: fileSize}, nil
		}
		// else, see if this is a file-not-found error
		if os.IsNotExist(openErr) {
			// the database told us that we have this file.. but we couldn't find it.
			// delete it from the database.
			crw, err2 := ct.dbs.MakeCatchpointReaderWriter()
			if err2 != nil {
				return nil, err2
			}
			err2 = ct.recordCatchpointFile(context.Background(), crw, round, "", 0)
			if err2 != nil {
				ct.log.Warnf("catchpointTracker.GetCatchpointStream() unable to delete missing catchpoint entry: %v", err2)
				return nil, err2
			}

			return nil, ledgercore.ErrNoEntry{}
		}
		// it's some other error.
		return nil, fmt.Errorf("catchpointTracker.GetCatchpointStream() unable to open catchpoint file '%s' %v", catchpointPath, openErr)
	}

	// if the database doesn't know about that round, see if we have that file anyway:
	relCatchpointFilePath := filepath.Join(trackerdb.CatchpointDirName, trackerdb.MakeCatchpointFilePath(round))
	absCatchpointFilePath := filepath.Join(ct.dbDirectory, relCatchpointFilePath)
	file, err := os.OpenFile(absCatchpointFilePath, os.O_RDONLY, 0666)
	if err == nil && file != nil {
		// great, if found that we should have had this in the database.. add this one now :
		fileInfo, err := file.Stat()
		if err != nil {
			// we couldn't get the stat, so just return with the file.
			return &readCloseSizer{ReadCloser: file, size: -1}, nil //nolint:nilerr // intentionally ignoring Stat error
		}
		crw, err := ct.dbs.MakeCatchpointReaderWriter()
		if err != nil {
			return nil, err
		}
		err = ct.recordCatchpointFile(context.Background(), crw, round, relCatchpointFilePath, fileInfo.Size())
		if err != nil {
			ct.log.Warnf("catchpointTracker.GetCatchpointStream() unable to save missing catchpoint entry: %v", err)
		}
		return &readCloseSizer{ReadCloser: file, size: fileInfo.Size()}, nil
	}
	return nil, ledgercore.ErrNoEntry{}
}

func (ct *catchpointTracker) catchpointEnabled() bool {
	return ct.catchpointInterval != 0
}

// initializeHashes initializes account/resource/kv hashes.
// as part of the initialization, it tests if a hash table matches to account base and updates the former.
func (ct *catchpointTracker) initializeHashes(ctx context.Context, tx trackerdb.TransactionScope, rnd basics.Round) error {
	ar, err := tx.MakeAccountsReader()
	if err != nil {
		return err
	}

	aw, err := tx.MakeAccountsWriter()
	if err != nil {
		return err
	}

	hashRound, err := ar.AccountsHashRound(ctx)
	if err != nil {
		return err
	}

	if hashRound != rnd {
		// if the hashed round is different then the base round, something was modified, and the accounts aren't in sync
		// with the hashes.
		err = aw.ResetAccountHashes(ctx)
		if err != nil {
			return err
		}
		// if catchpoint is disabled on this node, we could complete the initialization right here.
		if !ct.catchpointEnabled() {
			return nil
		}
	}

	// create the merkle trie for the balances
	committer, err := tx.MakeMerkleCommitter(false)
	if err != nil {
		return fmt.Errorf("initializeHashes was unable to makeMerkleCommitter: %v", err)
	}

	trie, err := merkletrie.MakeTrie(committer, trackerdb.TrieMemoryConfig)
	if err != nil {
		return fmt.Errorf("initializeHashes was unable to MakeTrie: %v", err)
	}

	// we might have a database that was previously initialized, and now we're adding the balances trie. In that case, we need to add all the existing balances to this trie.
	// we can figure this out by examining the hash of the root:
	rootHash, err := trie.RootHash()
	if err != nil {
		return fmt.Errorf("initializeHashes was unable to retrieve trie root hash: %v", err)
	}

	if rootHash.IsZero() {
		ct.log.Infof("initializeHashes rebuilding merkle trie for round %d", rnd)
		accountBuilderIt := tx.MakeOrderedAccountsIter(trieRebuildAccountChunkSize)
		defer accountBuilderIt.Close(ctx)
		startTrieBuildTime := time.Now()
		trieHashCount := 0
		lastRebuildTime := startTrieBuildTime
		pendingTrieHashes := 0
		totalOrderedAccounts := 0
		for {
			accts, processedRows, itErr := accountBuilderIt.Next(ctx)
			if itErr == sql.ErrNoRows {
				// the account builder would return sql.ErrNoRows when no more data is available.
				break
			} else if itErr != nil {
				return itErr
			}

			if len(accts) > 0 {
				trieHashCount += len(accts)
				pendingTrieHashes += len(accts)
				for _, acct := range accts {
					added, addErr := trie.Add(acct.Digest)
					if addErr != nil {
						return fmt.Errorf("initializeHashes was unable to add acct to trie: %v", addErr)
					}
					if !added {
						// we need to translate the "addrid" into actual account address so that
						// we can report the failure.
						addr, lErr := ar.LookupAccountAddressFromAddressID(ctx, acct.AccountRef)
						if lErr != nil {
							ct.log.Warnf("initializeHashes attempted to add duplicate acct hash '%s' to merkle trie for account id %d : %v", hex.EncodeToString(acct.Digest), acct.AccountRef, lErr)
						} else {
							ct.log.Warnf("initializeHashes attempted to add duplicate acct hash '%s' to merkle trie for account %v", hex.EncodeToString(acct.Digest), addr)
						}
					}
				}

				if pendingTrieHashes >= trieRebuildCommitFrequency {
					// this trie Evict will commit using the current transaction.
					// if anything goes wrong, it will still get rolled back.
					_, err = trie.Evict(true)
					if err != nil {
						return fmt.Errorf("initializeHashes was unable to commit changes to trie: %v", err)
					}
					pendingTrieHashes = 0
				}

				if time.Since(lastRebuildTime) > 5*time.Second {
					// let the user know that the trie is still being rebuilt.
					ct.log.Infof("initializeHashes still building the trie, and processed so far %d trie entries", trieHashCount)
					lastRebuildTime = time.Now()
				}
			} else if processedRows > 0 {
				totalOrderedAccounts += processedRows
				// if it's not ordered, we can ignore it for now; we'll just increase the counters and emit logs periodically.
				if time.Since(lastRebuildTime) > 5*time.Second {
					// let the user know that the trie is still being rebuilt.
					ct.log.Infof("initializeHashes still building the trie, and hashed so far %d accounts", totalOrderedAccounts)
					lastRebuildTime = time.Now()
				}
			}
		}

		// this trie Evict will commit using the current transaction.
		// if anything goes wrong, it will still get rolled back.
		_, err = trie.Evict(true)
		if err != nil {
			return fmt.Errorf("initializeHashes was unable to commit changes to trie: %v", err)
		}

		// Now add the kvstore hashes
		pendingTrieHashes = 0
		kvs, err := tx.MakeKVsIter(ctx)
		if err != nil {
			return err
		}
		defer kvs.Close()
		for kvs.Next() {
			k, v, err2 := kvs.KeyValue()
			if err2 != nil {
				return err2
			}
			hash := trackerdb.KvHashBuilderV6(string(k), v)
			trieHashCount++
			pendingTrieHashes++
			added, err2 := trie.Add(hash)
			if err2 != nil {
				return fmt.Errorf("initializeHashes was unable to add kv (key=%s) to trie: %v", hex.EncodeToString(k), err2)
			}
			if !added {
				ct.log.Warnf("initializeHashes attempted to add duplicate kv hash '%s' to merkle trie for key %s", hex.EncodeToString(hash), k)
			}
			if pendingTrieHashes >= trieRebuildCommitFrequency {
				// this trie Evict will commit using the current transaction.
				// if anything goes wrong, it will still get rolled back.
				_, err2 = trie.Evict(true)
				if err2 != nil {
					return fmt.Errorf("initializeHashes was unable to commit changes to trie: %v", err2)
				}
				pendingTrieHashes = 0
			}
			// We could insert code to report things every 5 seconds, like was done for accounts.
		}

		// this trie Evict will commit using the current transaction.
		// if anything goes wrong, it will still get rolled back.
		_, err = trie.Evict(true)
		if err != nil {
			return fmt.Errorf("initializeHashes was unable to commit changes to trie: %v", err)
		}

		// we've just updated the merkle trie, update the hashRound to reflect that.
		err = aw.UpdateAccountsHashRound(ctx, rnd)
		if err != nil {
			return fmt.Errorf("initializeHashes was unable to update the account hash round to %d: %v", rnd, err)
		}

		ct.log.Infof("initializeHashes rebuilt the merkle trie with %d entries in %v", trieHashCount, time.Since(startTrieBuildTime))
	}
	ct.balancesTrie = trie
	return nil
}
