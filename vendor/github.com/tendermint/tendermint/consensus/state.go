package consensus

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"reflect"
	"sync"
	"time"
	//"runtime/debug"

	"github.com/ebuchman/fail-test"
	p2p "github.com/tendermint/go-p2p"

	. "github.com/tendermint/go-common"
	cfg "github.com/tendermint/go-config"
	"github.com/tendermint/go-wire"
	"github.com/tendermint/tendermint/proxy"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/types"
	"github.com/tendermint/go-crypto"
	ep "github.com/tendermint/tendermint/epoch"
	"github.com/ethereum/go-ethereum/common"
)

const (
	newHeightChangeSleepDuration     = 2000 * time.Millisecond
	sendPrecommitSleepDuration       = 100 * time.Millisecond
	preProposeSleepDuration          = 120000 * time.Millisecond // Time to sleep before starting consensus.
)

//-----------------------------------------------------------------------------
// Timeout Parameters

// TimeoutParams holds timeouts and deltas for each round step.
// All timeouts and deltas in milliseconds.
type TimeoutParams struct {
	Propose0          int
	ProposeDelta      int
	Prevote0          int
	PrevoteDelta      int
	Precommit0        int
	PrecommitDelta    int
	Commit0           int
	SkipTimeoutCommit bool
}

// Wait this long for a proposal
func (tp *TimeoutParams) Propose(round int) time.Duration {
	return time.Duration(tp.Propose0+tp.ProposeDelta*round) * time.Millisecond
}

// After receiving any +2/3 prevote, wait this long for stragglers
func (tp *TimeoutParams) Prevote(round int) time.Duration {
	return time.Duration(tp.Prevote0+tp.PrevoteDelta*round) * time.Millisecond
}

// After receiving any +2/3 precommits, wait this long for stragglers
func (tp *TimeoutParams) Precommit(round int) time.Duration {
	return time.Duration(tp.Precommit0+tp.PrecommitDelta*round) * time.Millisecond
}

// After receiving +2/3 precommits for a single block (a commit), wait this long for stragglers in the next height's RoundStepNewHeight
func (tp *TimeoutParams) Commit(t time.Time) time.Time {
	return t.Add(time.Duration(tp.Commit0) * time.Millisecond)
}

// InitTimeoutParamsFromConfig initializes parameters from config
func InitTimeoutParamsFromConfig(config cfg.Config) *TimeoutParams {
	return &TimeoutParams{
		Propose0:          config.GetInt("timeout_propose"),
		ProposeDelta:      config.GetInt("timeout_propose_delta"),
		Prevote0:          config.GetInt("timeout_prevote"),
		PrevoteDelta:      config.GetInt("timeout_prevote_delta"),
		Precommit0:        config.GetInt("timeout_precommit"),
		PrecommitDelta:    config.GetInt("timeout_precommit_delta"),
		Commit0:           config.GetInt("timeout_commit"),
		SkipTimeoutCommit: config.GetBool("skip_timeout_commit"),
	}
}

//-----------------------------------------------------------------------------
// Errors

var (
	ErrInvalidProposalSignature = errors.New("Error invalid proposal signature")
	ErrInvalidProposalPOLRound  = errors.New("Error invalid proposal POL round")
	ErrAddingVote               = errors.New("Error adding vote")
	ErrVoteHeightMismatch       = errors.New("Error vote height mismatch")
	ErrInvalidSignatureAggr	    = errors.New("Invalid signature aggregation")
	ErrDuplicateSignatureAggr   = errors.New("Duplicate signature aggregation")
	ErrNotMaj23SignatureAggr    = errors.New("Signature aggregation has no +2/3 power")
)

//-----------------------------------------------------------------------------
// RoundStepType enum type

type RoundStepType uint8 // These must be numeric, ordered.

const (
	RoundStepNewHeight     = RoundStepType(0x01) // Wait til CommitTime + timeoutCommit
	RoundStepNewRound      = RoundStepType(0x02) // Setup new round and go to RoundStepPropose
	RoundStepPropose       = RoundStepType(0x03) // Did propose, gossip proposal
	RoundStepPrevote       = RoundStepType(0x04) // Did prevote, gossip prevotes
	RoundStepPrevoteWait   = RoundStepType(0x05) // Did receive any +2/3 prevotes, start timeout
	RoundStepPrecommit     = RoundStepType(0x06) // Did precommit, gossip precommits
	RoundStepPrecommitWait = RoundStepType(0x07) // Did receive any +2/3 precommits, start timeout
	RoundStepCommit        = RoundStepType(0x08) // Entered commit state machine
	RoundStepTest          = RoundStepType(0x09) // for test author@liaoyd
	// NOTE: RoundStepNewHeight acts as RoundStepCommitWait.
)

func (rs RoundStepType) String() string {
	switch rs {
	case RoundStepNewHeight:
		return "RoundStepNewHeight"
	case RoundStepNewRound:
		return "RoundStepNewRound"
	case RoundStepPropose:
		return "RoundStepPropose"
	case RoundStepPrevote:
		return "RoundStepPrevote"
	case RoundStepPrevoteWait:
		return "RoundStepPrevoteWait"
	case RoundStepPrecommit:
		return "RoundStepPrecommit"
	case RoundStepPrecommitWait:
		return "RoundStepPrecommitWait"
	case RoundStepCommit:
		return "RoundStepCommit"
	case RoundStepTest:
		return "RoundStepTest"
	default:
		return "RoundStepUnknown" // Cannot panic.
	}
}

//-----------------------------------------------------------------------------

// Immutable when returned from ConsensusState.GetRoundState()
// TODO: Actually, only the top pointer is copied,
// so access to field pointers is still racey
type RoundState struct {
	Height             int // Height we are working on
	Round              int
	Step               RoundStepType
	StartTime          time.Time
	CommitTime         time.Time // Subjective time when +2/3 precommits for Block at Round were found
	Epoch              *ep.Epoch
	Validators         *types.ValidatorSet
	Proposal           *types.Proposal
	ProposalBlock      *types.Block
	ProposalBlockParts *types.PartSet
	ProposerNetAddr	   string		// Proposer's IP address and port
	ProposerPeerKey	   string		// Proposer's peer key
	LockedRound        int
	LockedBlock        *types.Block
	LockedBlockParts   *types.PartSet
	Votes              *HeightVoteSet
	VoteSignAggr       *HeightVoteSignAggr
	CommitRound        int            //
	LastCommit         *types.SignAggr // Last precommits at Height-1
	LastValidators     *types.ValidatorSet

	// Following fields are used for BLS signature aggregation
	PrevoteMaj23SignAggr	*types.SignAggr
	PrecommitMaj23SignAggr	*types.SignAggr
}

func (rs *RoundState) RoundStateEvent() types.EventDataRoundState {
	edrs := types.EventDataRoundState{
		Height:     rs.Height,
		Round:      rs.Round,
		Step:       rs.Step.String(),
		RoundState: rs,
	}
	return edrs
}

func (rs *RoundState) String() string {
	return rs.StringIndented("")
}

func (rs *RoundState) StringIndented(indent string) string {
	return fmt.Sprintf(`RoundState{
%s  H:%v R:%v S:%v
%s  StartTime:     %v
%s  CommitTime:    %v
%s  Validators:    %v
%s  Proposal:      %v
%s  ProposalBlock: %v %v
%s  LockedRound:   %v
%s  LockedBlock:   %v %v
%s  Votes:         %v
%s  LastCommit: %v
%s  LastValidators:    %v
%s}`,
		indent, rs.Height, rs.Round, rs.Step,
		indent, rs.StartTime,
		indent, rs.CommitTime,
		indent, rs.Validators.StringIndented(indent+"    "),
		indent, rs.Proposal,
		indent, rs.ProposalBlockParts.StringShort(), rs.ProposalBlock.StringShort(),
		indent, rs.LockedRound,
		indent, rs.LockedBlockParts.StringShort(), rs.LockedBlock.StringShort(),
		indent, rs.Votes.StringIndented(indent+"    "),
		indent, rs.LastCommit.StringShort(),
		indent, rs.LastValidators.StringIndented(indent+"    "),
		indent)
}

func (rs *RoundState) StringShort() string {
	return fmt.Sprintf(`RoundState{H:%v R:%v S:%v ST:%v}`,
		rs.Height, rs.Round, rs.Step, rs.StartTime)
}

//-----------------------------------------------------------------------------

var (
	msgQueueSize = 1000
)

// msgs from the reactor which may update the state
type msgInfo struct {
	Msg     ConsensusMessage `json:"msg"`
	PeerKey string           `json:"peer_key"`
}

// internally generated messages which may update the state
type timeoutInfo struct {
	Duration time.Duration `json:"duration"`
	Height   int           `json:"height"`
	Round    int           `json:"round"`
	Step     RoundStepType `json:"step"`
}

func (ti *timeoutInfo) String() string {
	return fmt.Sprintf("%v ; %d/%d %v", ti.Duration, ti.Height, ti.Round, ti.Step)
}

// Tracks consensus state across block heights and rounds.
type PrivValidator interface {
	GetAddress() []byte
	GetPubKey() crypto.PubKey
	SignVote(chainID string, vote *types.Vote) error
	SignProposal(chainID string, proposal *types.Proposal) error
	SignValidatorMsg(chainID string, msg *types.ValidatorMsg) error
}

// Tracks consensus state across block heights and rounds.
type ConsensusState struct {
	BaseService

	config       cfg.Config
	proxyAppConn proxy.AppConnConsensus
	blockStore   types.BlockStore
	mempool      types.Mempool
	privValidator PrivValidator // for signing votes

	nodeInfo	*p2p.NodeInfo	// Validator's node info (ip, port, etc)

	mtx sync.Mutex
	RoundState
	epoch *ep.Epoch
	state *sm.State // State until height-1.

	peerMsgQueue     chan msgInfo   // serializes msgs affecting state (proposals, block parts, votes)
	internalMsgQueue chan msgInfo   // like peerMsgQueue but for our own proposals, parts, votes
	timeoutTicker    TimeoutTicker  // ticker for timeouts
	timeoutParams    *TimeoutParams // parameters and functions for timeout intervals

	evsw types.EventSwitch

	wal        *WAL
	replayMode bool // so we don't log signing errors during replay

	nSteps int // used for testing to limit the number of transitions the state makes

	// allow certain function to be overwritten for testing
	decideProposal func(height, round int)
	doPrevote      func(height, round int)
	setProposal    func(proposal *types.Proposal) error

	done chan struct{}
}

func NewConsensusState(config cfg.Config, state *sm.State, proxyAppConn proxy.AppConnConsensus,
	blockStore types.BlockStore, mempool types.Mempool, epoch *ep.Epoch) *ConsensusState {
	// fmt.Println("state.Validator in newconsensus:", state.Validators)
	cs := &ConsensusState{
		config:           config,
		proxyAppConn:     proxyAppConn,
		blockStore:       blockStore,
		mempool:          mempool,
		peerMsgQueue:     make(chan msgInfo, msgQueueSize),
		internalMsgQueue: make(chan msgInfo, msgQueueSize),
		timeoutTicker:    NewTimeoutTicker(),
		timeoutParams:    InitTimeoutParamsFromConfig(config),
		done:             make(chan struct{}),
	}
	// set function defaults (may be overwritten before calling Start)
	cs.decideProposal = cs.defaultDecideProposal
	//cs.doPrevote = cs.defaultDoPrevote
	cs.doPrevote = cs.newDoPrevote
	//cs.setProposal = cs.defaultSetProposal
	cs.setProposal = cs.newSetProposal

	cs.updateToStateAndEpoch(state, epoch)

	// Don't call scheduleRound0 yet.
	// We do that upon Start().
	cs.reconstructLastCommit(state)
	cs.BaseService = *NewBaseService(logger, "ConsensusState", cs)
	return cs
}

//----------------------------------------
// Public interface

// SetEventSwitch implements events.Eventable
func (cs *ConsensusState) SetEventSwitch(evsw types.EventSwitch) {
	cs.evsw = evsw
}

func (cs *ConsensusState) String() string {
	// better not to access shared variables
	return Fmt("ConsensusState") //(H:%v R:%v S:%v", cs.Height, cs.Round, cs.Step)
}

func (cs *ConsensusState) GetState() *sm.State {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	return cs.state.Copy()
}

func (cs *ConsensusState) GetRoundState() *RoundState {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	return cs.getRoundState()
}

func (cs *ConsensusState) getRoundState() *RoundState {
	rs := cs.RoundState // copy
	return &rs
}

func (cs *ConsensusState) GetValidators() (int, []*types.Validator) {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()

	_, val, _ := cs.state.GetValidators()
	return cs.state.LastBlockHeight, val.Copy().Validators
}

// Sets our private validator account for signing votes.
func (cs *ConsensusState) SetPrivValidator(priv PrivValidator) {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	cs.privValidator = priv
}

// Sets our private validator account for signing votes.
func (cs *ConsensusState) GetProposer() (*types.Validator) {
	return cs.Validators.GetProposer()
}

// Returns true if this validator is the proposer.
func (cs *ConsensusState) IsProposer() bool {
	if bytes.Equal(cs.Validators.GetProposer().Address, cs.privValidator.GetAddress()) {
		return true
	} else {
		return false
	}
}

// Set the local timer
func (cs *ConsensusState) SetTimeoutTicker(timeoutTicker TimeoutTicker) {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	cs.timeoutTicker = timeoutTicker
}

func (cs *ConsensusState) LoadCommit(height int) *types.Commit {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	if height == cs.blockStore.Height() {
		return cs.blockStore.LoadSeenCommit(height)
	}
	return cs.blockStore.LoadBlockCommit(height)
}

func (cs *ConsensusState) OnStart() error {
	if cs.nodeInfo == nil {
		panic("cs.nodeInfo is nil\n")
	}

	walFile := cs.config.GetString("cs_wal_file")
	if err := cs.OpenWAL(walFile); err != nil {
		logger.Error("Error loading ConsensusState wal", " error:", err.Error())
		return err
	}

	// we need the timeoutRoutine for replay so
	//  we don't block on the tick chan.
	// NOTE: we will get a build up of garbage go routines
	//  firing on the tockChan until the receiveRoutine is started
	//  to deal with them (by that point, at most one will be valid)
	cs.timeoutTicker.Start()

	// we may have lost some votes if the process crashed
	// reload from consensus log to catchup
	if err := cs.catchupReplay(cs.Height); err != nil {
		logger.Error("Error on catchup replay. Proceeding to start ConsensusState anyway", " error:", err.Error())
		// NOTE: if we ever do return an error here,
		// make sure to stop the timeoutTicker
	}

	// now start the receiveRoutine
	go cs.receiveRoutine(0)

	// schedule the first round!
	// use GetRoundState so we don't race the receiveRoutine for access
	cs.scheduleRound0(cs.GetRoundState())

	return nil
}

// timeoutRoutine: receive requests for timeouts on tickChan and fire timeouts on tockChan
// receiveRoutine: serializes processing of proposoals, block parts, votes; coordinates state transitions
func (cs *ConsensusState) startRoutines(maxSteps int) {
	cs.timeoutTicker.Start()
	go cs.receiveRoutine(maxSteps)
}

func (cs *ConsensusState) OnStop() {
	cs.BaseService.OnStop()

	cs.timeoutTicker.Stop()

	// Make BaseService.Wait() wait until cs.wal.Wait()
	if cs.wal != nil && cs.IsRunning() {
		cs.wal.Wait()
	}
}

// NOTE: be sure to Stop() the event switch and drain
// any event channels or this may deadlock
func (cs *ConsensusState) Wait() {
	<-cs.done
}

// Open file to log all consensus messages and timeouts for deterministic accountability
func (cs *ConsensusState) OpenWAL(walFile string) (err error) {
	err = EnsureDir(path.Dir(walFile), 0700)
	if err != nil {
		logger.Error("Error ensuring ConsensusState wal dir", " error:", err.Error())
		return err
	}

	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	wal, err := NewWAL(walFile, cs.config.GetBool("cs_wal_light"))
	if err != nil {
		return err
	}
	cs.wal = wal
	return nil
}

//------------------------------------------------------------
// Public interface for passing messages into the consensus state,
// possibly causing a state transition
// TODO: should these return anything or let callers just use events?

// May block on send if queue is full.
func (cs *ConsensusState) AddVote(vote *types.Vote, peerKey string) (added bool, err error) {
	if peerKey == "" {
		cs.internalMsgQueue <- msgInfo{&VoteMessage{vote}, ""}
	} else {
		cs.peerMsgQueue <- msgInfo{&VoteMessage{vote}, peerKey}
	}

	// TODO: wait for event?!
	return false, nil
}

// May block on send if queue is full.
func (cs *ConsensusState) SetProposal(proposal *types.Proposal, peerKey string) error {

	if peerKey == "" {
		cs.internalMsgQueue <- msgInfo{&ProposalMessage{proposal}, ""}
	} else {
		cs.peerMsgQueue <- msgInfo{&ProposalMessage{proposal}, peerKey}
	}

	// TODO: wait for event?!
	return nil
}

// May block on send if queue is full.
func (cs *ConsensusState) AddProposalBlockPart(height, round int, part *types.Part, peerKey string) error {

	if peerKey == "" {
		cs.internalMsgQueue <- msgInfo{&BlockPartMessage{height, round, part}, ""}
	} else {
		cs.peerMsgQueue <- msgInfo{&BlockPartMessage{height, round, part}, peerKey}
	}

	// TODO: wait for event?!
	return nil
}

// May block on send if queue is full.
func (cs *ConsensusState) SetProposalAndBlock(proposal *types.Proposal, block *types.Block, parts *types.PartSet, peerKey string) error {
	cs.SetProposal(proposal, peerKey)
	for i := 0; i < parts.Total(); i++ {
		part := parts.GetPart(i)
		cs.AddProposalBlockPart(proposal.Height, proposal.Round, part, peerKey)
	}
	return nil // TODO errors
}

// Set node info wich is about current validator's peer info
func (cs *ConsensusState) SetNodeInfo(nodeInfo *p2p.NodeInfo) {
	cs.nodeInfo = nodeInfo
}

//------------------------------------------------------------
// internal functions for managing the state

func (cs *ConsensusState) updateHeight(height int) {
	cs.Height = height
}

func (cs *ConsensusState) updateRoundStep(round int, step RoundStepType) {
	cs.Round = round
	cs.Step = step
}

// enterNewRound(height, 0) at cs.StartTime.
func (cs *ConsensusState) scheduleRound0(rs *RoundState) {
	//logger.Info("scheduleRound0", "now", time.Now(), "startTime", cs.StartTime)
	sleepDuration := rs.StartTime.Sub(time.Now())
	cs.scheduleTimeout(sleepDuration, rs.Height, 0, RoundStepNewHeight)
}

// Attempt to schedule a timeout (by sending timeoutInfo on the tickChan)
func (cs *ConsensusState) scheduleTimeout(duration time.Duration, height, round int, step RoundStepType) {
	cs.timeoutTicker.ScheduleTimeout(timeoutInfo{duration, height, round, step})
}

// send a msg into the receiveRoutine regarding our own proposal, block part, or vote
func (cs *ConsensusState) sendInternalMessage(mi msgInfo) {
	select {
	case cs.internalMsgQueue <- mi:
	default:
		// NOTE: using the go-routine means our votes can
		// be processed out of order.
		// TODO: use CList here for strict determinism and
		// attempt push to internalMsgQueue in receiveRoutine
		logger.Warn("Internal msg queue is full. Using a go-routine")
		go func() { cs.internalMsgQueue <- mi }()
	}
}

// Reconstruct LastCommit from SeenCommit, which we saved along with the block,
// (which happens even before saving the state)
func (cs *ConsensusState) reconstructLastCommit(state *sm.State) {
	if state.LastBlockHeight == 0 {
		return
	}
	seenCommit := cs.blockStore.LoadSeenCommit(state.LastBlockHeight)
	lastValidators, _, _ := state.GetValidators()

/*
	lastPrecommits := types.NewVoteSet(cs.config.GetString("chain_id"), state.LastBlockHeight, seenCommit.Round(), types.VoteTypePrecommit, lastValidators)

	fmt.Printf("seenCommit are: %v\n", seenCommit)
	fmt.Printf("lastPrecommits are: %v\n", lastPrecommits)

	for _, precommit := range seenCommit.Precommits {
		if precommit == nil {
			continue
		}
		added, err := lastPrecommits.AddVote(precommit)
		if !added || err != nil {
			PanicCrisis(Fmt("Failed to reconstruct LastCommit: %v", err))
		}
	}
*/

	if seenCommit.Size() != lastValidators.Size() {
		panic("size of lastValidators is not equal to that saved in last commit")
	}

	lastPrecommits := types.MakeSignAggr(seenCommit.Height,
				       seenCommit.Round,
				       types.VoteTypePrecommit,
				       seenCommit.Size(),
				       seenCommit.BlockID,
				       state.ChainID,
				       seenCommit.BitArray.Copy(),
				       seenCommit.SignAggr)

//	if !lastPrecommits.HasTwoThirdsMajority() {
//		PanicSanity("Failed to reconstruct LastCommit: Does not have +2/3 maj")
//	}
	cs.LastCommit = lastPrecommits
}

// Updates ConsensusState and increments height to match thatRewardScheme of state.
// The round becomes 0 and cs.Step becomes RoundStepNewHeight.
func (cs *ConsensusState) updateToStateAndEpoch(state *sm.State, epoch *ep.Epoch) {
	var lastPrecommits *types.SignAggr = nil

	if cs.CommitRound > -1 && 0 < cs.Height && cs.Height != state.LastBlockHeight {
		PanicSanity(Fmt("updateToState() expected state height of %v but found %v",
			cs.Height, state.LastBlockHeight))
	}
	if cs.state != nil && cs.state.LastBlockHeight+1 != cs.Height {
		// This might happen when someone else is mutating cs.state.
		// Someone forgot to pass in state.Copy() somewhere?!
		PanicSanity(Fmt("Inconsistent cs.state.LastBlockHeight+1 %v vs cs.Height %v",
			cs.state.LastBlockHeight+1, cs.Height))
	}

	// If state isn't further out than cs.state, just ignore.
	// This happens when SwitchToConsensus() is called in the reactor.
	// We don't want to reset e.g. the Votes.
	if cs.state != nil && (state.LastBlockHeight <= cs.state.LastBlockHeight) {
		logger.Info("Ignoring updateToState()", " newHeight:", state.LastBlockHeight+1, " oldHeight:", cs.state.LastBlockHeight+1)
		return
	}

	// Reset fields based on state.
	_, validators, _ := state.GetValidators()
	//liaoyd
	// fmt.Println("validators:", validators)

	if cs.CommitRound > -1 && cs.VoteSignAggr != nil {
//		if !cs.VoteSignAggr.Precommits(cs.CommitRound).HasTwoThirdsMajority() {
//			PanicSanity("updateToState(state) called but last Precommit round didn't have +2/3")
//		}

		lastPrecommits = cs.VoteSignAggr.Precommits(cs.CommitRound)
	}

	// Next desired block height
	height := state.LastBlockHeight + 1

	// RoundState fields
	cs.updateHeight(height)
	cs.updateRoundStep(0, RoundStepNewHeight)
	if cs.CommitTime.IsZero() {
		// "Now" makes it easier to sync up dev nodes.
		// We add timeoutCommit to allow transactions
		// to be gathered for the first block.
		// And alternative solution that relies on clocks:
		//  cs.StartTime = state.LastBlockTime.Add(timeoutCommit)
		cs.StartTime = cs.timeoutParams.Commit(time.Now())
	} else {
		cs.StartTime = cs.timeoutParams.Commit(cs.CommitTime)
	}
	cs.Validators = validators
	cs.Proposal = nil
	cs.ProposalBlock = nil
	cs.ProposalBlockParts = nil
	cs.PrevoteMaj23SignAggr = nil
	cs.PrecommitMaj23SignAggr = nil
	cs.LockedRound = 0
	cs.LockedBlock = nil
	cs.LockedBlockParts = nil
	cs.Votes = NewHeightVoteSet(cs.config.GetString("chain_id"), height, validators)
	cs.VoteSignAggr = NewHeightVoteSignAggr(cs.config.GetString("chain_id"), height, validators)
	cs.CommitRound = -1
	cs.LastCommit = lastPrecommits
	cs.Epoch = epoch

	//fmt.Printf("State.Copy(), cs.LastValidators are: %v, state.LastValidators are: %v\n",
	//	cs.LastValidators, state.LastValidators)
	//debug.PrintStack()

	cs.LastValidators, _, _ = state.GetValidators()

	cs.state = state

	cs.epoch = epoch

	cs.newStep()
}

func (cs *ConsensusState) newStep() {
	rs := cs.RoundStateEvent()
	cs.wal.Save(rs)
	cs.nSteps += 1
	// newStep is called by updateToStep in NewConsensusState before the evsw is set!
	if cs.evsw != nil {
		types.FireEventNewRoundStep(cs.evsw, rs)
	}
}

//-----------------------------------------
// the main go routines

// receiveRoutine handles messages which may cause state transitions.
// it's argument (n) is the number of messages to process before exiting - use 0 to run forever
// It keeps the RoundState and is the only thing that updates it.
// Updates (state transitions) happen on timeouts, complete proposals, and 2/3 majorities
func (cs *ConsensusState) receiveRoutine(maxSteps int) {
	for {
		if maxSteps > 0 {
			if cs.nSteps >= maxSteps {
				logger.Warn("reached max steps. exiting receive routine")
				cs.nSteps = 0
				return
			}
		}
		rs := cs.RoundState
		var mi msgInfo

		select {
		case mi = <-cs.peerMsgQueue:
			logger.Debug(Fmt("Got msg from peer queue: %+v\n", mi))
			cs.wal.Save(mi)
			// handles proposals, block parts, votes
			// may generate internal events (votes, complete proposals, 2/3 majorities)
			cs.handleMsg(mi, rs)
		case mi = <-cs.internalMsgQueue:
			logger.Debug(Fmt("Got msg from internal queue %+v\n", mi))
			cs.wal.Save(mi)
			// handles proposals, block parts, votes
			cs.handleMsg(mi, rs)
		case ti := <-cs.timeoutTicker.Chan(): // tockChan:
			cs.wal.Save(ti)
			// if the timeout is relevant to the rs
			// go to the next step
			cs.handleTimeout(ti, rs)
		case <-cs.Quit:

			// NOTE: the internalMsgQueue may have signed messages from our
			// priv_val that haven't hit the WAL, but its ok because
			// priv_val tracks LastSig

			// close wal now that we're done writing to it
			if cs.wal != nil {
				cs.wal.Stop()
			}

			close(cs.done)
			return
		}
	}
}

// state transitions on complete-proposal, 2/3-any, 2/3-one
func (cs *ConsensusState) handleMsg(mi msgInfo, rs RoundState) {
//	cs.mtx.Lock()
//	defer cs.mtx.Unlock()

	var err error
	msg, peerKey := mi.Msg, mi.PeerKey
	switch msg := msg.(type) {
	case *ProposalMessage:
		// will not cause transition.
		// once proposal is set, we can receive block parts
		logger.Debug(Fmt("handleMsg: Received proposal message %+v\n", msg))
		cs.mtx.Lock()
		err = cs.setProposal(msg.Proposal)
		cs.mtx.Unlock()
		if err == nil {
			// enterPrevote don't wait for complete block
			cs.enterPrevote(cs.Height, cs.Round)
		}
	case *BlockPartMessage:
		// if the proposal is complete, we'll enterPrevote or tryFinalizeCommit
		logger.Error(Fmt("handleMsg: Received proposal block part message %+v\n", msg.Part))
		cs.mtx.Lock()
		_, err = cs.addProposalBlockPart(msg.Height, msg.Part, peerKey != "")
		cs.mtx.Unlock()
		if err != nil && msg.Round != cs.Round {
			err = nil
		}
		if err != nil {
			logger.Error(Fmt("add block part err:%v", err))
		}
		cs.mtx.Lock()
		if err == nil && cs.isProposalComplete() && cs.Step == RoundStepPrevote {
			sign_aggr := cs.VoteSignAggr.getSignAggr(cs.Round, types.VoteTypePrevote)
			if sign_aggr != nil && sign_aggr.HasTwoThirdsMajority(cs.Validators) {
				cs.enterPrecommit(cs.Height, cs.Round)
			}
		}
		cs.mtx.Unlock()
	case *Maj23SignAggrMessage:
		// Msg saying a set of 2/3+ signatures had been received

		logger.Debug(Fmt("handleMsg: Received Maj23SignAggrMessage %#v\n", (msg.Maj23SignAggr)))
		logger.Error(Fmt("handleMsg: type%v\n", msg.Maj23SignAggr.Type))
		if msg.Maj23SignAggr.Type == types.VoteTypePrecommit {
			logger.Error("Maj23SignAggrMessage")
		}
		cs.mtx.Lock()
//		enterNext := false
		err, _ = cs.setMaj23SignAggr(msg.Maj23SignAggr)
		cs.mtx.Unlock()
/*
		if err == nil && enterNext {
			if msg.Maj23SignAggr.Type == types.VoteTypePrevote {
				cs.enterPrecommit(cs.Height, cs.Round)
			} else if msg.Maj23SignAggr.Type == types.VoteTypePrecommit {
				cs.enterCommit(cs.Height, cs.Round)
			}
		}
*/
	case *VoteMessage:
		// attempt to add the vote and dupeout the validator if its a duplicate signature
		// if the vote gives us a 2/3-any or 2/3-one, we transition
		cs.mtx.Lock()
		err := cs.tryAddVote(msg.Vote, peerKey)
		cs.mtx.Unlock()
		if err == ErrAddingVote {
			// TODO: punish peer
		}
		if cs.PrecommitMaj23SignAggr != nil {
                        fmt.Println("Sleeping 100ms waiting for sending sign aggr ")
                        time.Sleep(sendPrecommitSleepDuration)
		}

		// NOTE: the vote is broadcast to peers by the reactor listening
		// for vote events

		// TODO: If rs.Height == vote.Height && rs.Round < vote.Round,
		// the peer is sending us CatchupCommit precommits.
		// We could make note of this and help filter in broadcastHasVoteMessage().
	default:
		logger.Warn("Unknown msg type ", reflect.TypeOf(msg))
	}
	if err != nil {
		logger.Error("Error with msg", " type:", reflect.TypeOf(msg), " peer:", peerKey, " error:", err, " msg:", msg)
	}
}

func (cs *ConsensusState) handleTimeout(ti timeoutInfo, rs RoundState) {
	logger.Debug("Received tock", " timeout:", ti.Duration, " height:", ti.Height, " round:", ti.Round, " step:", ti.Step)

	// timeouts must be for current height, round, step
	if ti.Height != rs.Height || ti.Round < rs.Round || (ti.Round == rs.Round && ti.Step < rs.Step) {
		logger.Debug("Ignoring tock because we're ahead", " height:", rs.Height, " round:", rs.Round, " step:", rs.Step)
		return
	}

	// the timeout will now cause a state transition
	cs.mtx.Lock()
	defer cs.mtx.Unlock()

	switch ti.Step {
	case RoundStepNewHeight:
		// NewRound event fired from enterNewRound.
		// XXX: should we fire timeout here (for timeout commit)?
		cs.enterNewRound(ti.Height, 0)
	case RoundStepPropose:
		types.FireEventTimeoutPropose(cs.evsw, cs.RoundStateEvent())
		cs.enterPrevote(ti.Height, ti.Round)
	case RoundStepPrevoteWait:
		types.FireEventTimeoutWait(cs.evsw, cs.RoundStateEvent())
		cs.enterPrecommit(ti.Height, ti.Round)
	case RoundStepPrecommitWait:
		types.FireEventTimeoutWait(cs.evsw, cs.RoundStateEvent())
		cs.enterNewRound(ti.Height, ti.Round+1)
	default:
		panic(Fmt("Invalid timeout step: %v", ti.Step))
	}

}

//-----------------------------------------------------------------------------
// State functions
// Used internally by handleTimeout and handleMsg to make state transitions

// Enter: +2/3 precommits for nil at (height,round-1)
// Enter: `timeoutPrecommits` after any +2/3 precommits from (height,round-1)
// Enter: `startTime = commitTime+timeoutCommit` from NewHeight(height)
// NOTE: cs.StartTime was already set for height.
func (cs *ConsensusState) enterNewRound(height int, round int) {
	if cs.Height != height || round < cs.Round || (cs.Round == round && cs.Step != RoundStepNewHeight) {
		logger.Debug(Fmt("enterNewRound(%v/%v): Invalid args. Current step: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))
		return
	}



	if now := time.Now(); cs.StartTime.After(now) {
		logger.Warn("Need to set a buffer and logger.Warn() here for sanity.", "startTime", cs.StartTime, "now", now)
	}

	logger.Info(Fmt("enterNewRound(%v/%v). Current: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))

	//liaoyd
	// fmt.Println("in func (cs *ConsensusState) enterNewRound(height int, round int)")
	fmt.Println(cs.Validators)
	// Increment validators if necessary
	validators := cs.Validators
	if cs.Round < round {
		validators = validators.Copy()
		validators.IncrementAccum(round - cs.Round)
	}

	// Setup new round
	// we don't fire newStep for this step,
	// but we fire an event, so update the round step first
	cs.updateRoundStep(round, RoundStepNewRound)
	cs.Validators = validators
	if round == 0 {
		// We've already reset these upon new height,
		// and meanwhile we might have received a proposal
		// for round 0.
	} else {
		cs.Proposal = nil
		cs.ProposalBlock = nil
		cs.ProposalBlockParts = nil
		cs.PrevoteMaj23SignAggr = nil
		cs.PrecommitMaj23SignAggr = nil
	}
	// cs.Votes.SetRound(round + 1) // also track next round (round+1) to allow round-skipping
	cs.VoteSignAggr.SetRound(round + 1) // also track next round (round+1) to allow round-skipping

	types.FireEventNewRound(cs.evsw, cs.RoundStateEvent())

	// Immediately go to enterPropose.
	cs.enterPropose(height, round)
}

// Enter: from NewRound(height,round).
func (cs *ConsensusState) enterPropose(height int, round int) {
	if cs.Height != height || round < cs.Round || (cs.Round == round && RoundStepPropose <= cs.Step) {
		logger.Debug(Fmt("enterPropose(%v/%v): Invalid args. Current step: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))
		return
	}
	logger.Info(Fmt("enterPropose(%v/%v). Current: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))

	defer func() {
		// Done enterPropose:
		cs.updateRoundStep(round, RoundStepPropose)
		cs.newStep()

		// If we have the whole proposal + POL, then goto Prevote now.
		// else, we'll enterPrevote when the rest of the proposal is received (in AddProposalBlockPart),
		// or else after timeoutPropose
		//if cs.isProposalComplete() {
		//	cs.enterPrevote(height, cs.Round)
		//}
		// enter prevote without waiting for complete block
		if cs.Proposal != nil {
			cs.enterPrevote(height, cs.Round)
		}

	}()

	// If we don't get the proposal and all block parts quick enough, enterPrevote
	cs.scheduleTimeout(cs.timeoutParams.Propose(round), height, round, RoundStepPropose)

	// Nothing more to do if we're not a validator
	if cs.privValidator == nil {
		fmt.Println("we are not validator yet!!!!!!!!saaaaaaad")
		return
	}

	if !bytes.Equal(cs.Validators.GetProposer().Address, cs.privValidator.GetAddress()) {
		fmt.Println("we are not proposer!!!")
		logger.Info("enterPropose: Not our turn to propose", "proposer", cs.Validators.GetProposer().Address, "privValidator", cs.privValidator)
	} else {
		fmt.Println("we are proposer!!!")
		logger.Info("enterPropose: Our turn to propose", "proposer", cs.Validators.GetProposer().Address, "privValidator", cs.privValidator)
		cs.decideProposal(height, round)

	}
}

func (cs *ConsensusState) defaultDecideProposal(height, round int) {
	var block *types.Block
	var blockParts *types.PartSet
	var proposerNetAddr  string
	var proposerPeerKey string

//logger.Debug(Fmt("defaultDecideProposal: ConsensusState %+v\n", cs))

	// Decide on block
	if cs.LockedBlock != nil {
		// If we're locked onto a block, just choose that.
		block, blockParts = cs.LockedBlock, cs.LockedBlockParts
	} else {
		// Create a new proposal block from state/txs from the mempool.
		block, blockParts = cs.createProposalBlock()
		if block == nil { // on error
			return
		}
	}

	// Get IP and pub key of current validators from nodeInfo
	if cs.nodeInfo != nil {
		proposerNetAddr = cs.nodeInfo.ListenAddres()
		proposerPeerKey = cs.nodeInfo.PubKey.KeyString()
	} else {
		panic("cs.nodeInfo is nil when decide the next block\n")
	}

	// fmt.Println("defaultDecideProposal: cs nodeInfo %#v\n", cs.nodeInfo)
	logger.Debug(Fmt("defaultDecideProposal: Proposer (ip %s peer key %s)", proposerNetAddr, proposerPeerKey))

	// Make proposal
	polRound, polBlockID := cs.VoteSignAggr.POLInfo()
	proposal := types.NewProposal(height, round, block.Hash(), blockParts.Header(), polRound, polBlockID, proposerNetAddr, proposerPeerKey)
	err := cs.privValidator.SignProposal(cs.state.ChainID, proposal)
	if err == nil {
		// Set fields
		/*  fields set by setProposal and addBlockPart
		cs.Proposal = proposal
		cs.ProposalBlock = block
		cs.ProposalBlockParts = blockParts
		cs.ProposerPeerKey = proposerPeerKey
		*/

		// send proposal and block parts on internal msg queue
		proposal_blockParts := types.EventDataProposalBlockParts{ proposal, blockParts}
		types.FireEventProposalBlockParts(cs.evsw, proposal_blockParts)
/*
		proposalMsg :=  types.EventDataProposal{proposal}
		types.FireEventProposal(cs.evsw, proposalMsg)
*/
		cs.sendInternalMessage(msgInfo{&ProposalMessage{proposal}, ""})

		//logger.Debug(Fmt("defaultDecideProposal: Proposal to send is %#v\n", proposal))
		//logger.Debug(Fmt("ProposalMessage to send is %+v\n", msgInfo{&ProposalMessage{proposal}, ""}))

		for i := 0; i < blockParts.Total(); i++ {
			part := blockParts.GetPart(i)
/*
			partMsg := types.EventDataBlockPart{cs.Round, cs.Height, part}
			types.FireEventBlockPart(cs.evsw, partMsg)
*/
			cs.sendInternalMessage(msgInfo{&BlockPartMessage{cs.Height, cs.Round, part}, ""})
		}
		logger.Info("Signed proposal", " height:", height, " round:", round, " proposal:", proposal)
	} else {
		if !cs.replayMode {
			logger.Warn("enterPropose: Error signing proposal", " height:", height, " round:", round, " error:", err)
		}
	}
}

// Returns true if the proposal block is complete &&
// (if POLRound was proposed, we have +2/3 prevotes from there).
func (cs *ConsensusState) isProposalComplete() bool {
	if cs.Proposal == nil || cs.ProposalBlock == nil {
		logger.Error("first step")
		return false
	}
	// we have the proposal. if there's a POLRound,
	// make sure we have the prevotes from it too
	if cs.Proposal.POLRound < 0 {
		return true
	} else {
		logger.Error("second step")
		// if this is false the proposer is lying or we haven't received the POL yet
		return cs.VoteSignAggr.Prevotes(cs.Proposal.POLRound).HasTwoThirdsMajority(cs.Validators)
	}
}

// Create the next block to propose and return it.
// Returns nil block upon error.
// NOTE: keep it side-effect free for clarity.
func (cs *ConsensusState) createProposalBlock() (block *types.Block, blockParts *types.PartSet) {
	var commit *types.Commit
	if cs.Height == 1 {
		// We're creating a proposal for the first block.
		// The commit is empty, but not nil.
		commit = &types.Commit{}
	} else if cs.LastCommit.HasTwoThirdsMajority(cs.Validators) {
		// Make the commit from LastCommit
		commit = cs.LastCommit.MakeCommit()
	} else {
		// This shouldn't happen.
		logger.Error("enterPropose: Cannot propose anything: No commit for the previous block.")
		return

//		//Don't throw error now, the last commits may be replaced with signature aggregation later
//		commit = &types.Commit{}
	}

	// Mempool validated transactions
	txs := cs.mempool.Reap(cs.config.GetInt("block_size"))

	epTxs, err := cs.Epoch.ProposeTransactions("proposer", cs.Height)
	if err != nil {
		return nil, nil
	}

	if len(epTxs) != 0 {
		fmt.Printf("createProposalBlock(), epoch propose %v txs\n", len(epTxs))
		txs = append(txs, epTxs...)
	}

	var epochBytes []byte = []byte{}
	shouldProposeEpoch := cs.Epoch.ShouldProposeNextEpoch(cs.Height)
	if shouldProposeEpoch {
		cs.Epoch.SetNextEpoch(cs.Epoch.ProposeNextEpoch(cs.Height))
		epochBytes = cs.Epoch.NextEpoch.Bytes()
	}

	_, val, _ := cs.state.GetValidators()

	return types.MakeBlock(cs.Height, cs.state.ChainID, txs, commit,
		cs.state.LastBlockID, val.Hash(), cs.state.AppHash,
		epochBytes, cs.config.GetInt("block_part_size"))
}

// Enter: `timeoutPropose` after entering Propose.
// Enter: proposal block and POL is ready.
// Enter: any +2/3 prevotes for future round.
// Prevote for LockedBlock if we're locked, or ProposalBlock if valid.
// Otherwise vote nil.
func (cs *ConsensusState) enterPrevote(height int, round int) {
	if cs.Height != height || round < cs.Round || (cs.Round == round && RoundStepPrevote <= cs.Step) {
		logger.Debug(Fmt("enterPrevote(%v/%v): Invalid args. Current step: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))
		return
	}

	defer func() {
		// Done enterPrevote:
		cs.updateRoundStep(round, RoundStepPrevote)
		cs.newStep()
	}()

	// fire event for how we got here
	//if cs.isProposalComplete() {
	//	types.FireEventCompleteProposal(cs.evsw, cs.RoundStateEvent())
	//} else {
	//	// we received +2/3 prevotes for a future round
	//	// TODO: catchup event?
	//}


	//??
	if cs.Proposal == nil {
		types.FireEventCompleteProposal(cs.evsw, cs.RoundStateEvent())
	} else {
		// we received +2/3 prevotes for a future round
		// TODO: catchup event?
	}

	logger.Info(Fmt("enterPrevote(%v/%v). Current: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))

	// Sign and broadcast vote as necessary
	cs.doPrevote(height, round)

	// Once `addVote` hits any +2/3 prevotes, we will go to PrevoteWait
	// (so we have more time to try and collect +2/3 prevotes for a single block)
}

func (cs *ConsensusState) newDoPrevote(height int, round int) {
	// If a block is locked, prevote that.
	if cs.LockedBlock != nil {
		logger.Info("enterPrevote: Block was locked")
		cs.signAddVote(types.VoteTypePrevote, cs.LockedBlock.Hash().Bytes(), cs.LockedBlockParts.Header())
		return
	}

	// If Proposal is nil, prevote nil.
	if cs.Proposal == nil {
		logger.Warn("enterPrevote: ProposalBlock is nil")
		cs.signAddVote(types.VoteTypePrevote, nil, types.PartSetHeader{})
		return
	}

	// NOTE: Don't valdiate proposal block
	// Prevote cs.ProposalBlock
	// NOTE: the proposal signature is validated when it is received,
	// and the proposal block parts are validated as they are received (against the merkle hash in the proposal)
	//cs.signAddVote(types.VoteTypePrevote, cs.ProposalBlock.Hash().Bytes(), cs.ProposalBlockParts.Header())
	logger.Error(cs.Proposal.BlockHeaderHash())
	logger.Error(cs.Proposal.Hash)
	logger.Error(cs.Proposal.BlockPartsHeader)
	cs.signAddVote(types.VoteTypePrevote, cs.Proposal.BlockHeaderHash().Bytes(), cs.Proposal.BlockPartsHeader)
	return
}

func (cs *ConsensusState) defaultDoPrevote(height int, round int) {
	// If a block is locked, prevote that.
	if cs.LockedBlock != nil {
		logger.Info("enterPrevote: Block was locked")
		cs.signAddVote(types.VoteTypePrevote, cs.LockedBlock.Hash().Bytes(), cs.LockedBlockParts.Header())
		return
	}

	// If ProposalBlock is nil, prevote nil.
	if cs.ProposalBlock == nil {
		logger.Warn("enterPrevote: ProposalBlock is nil")
		cs.signAddVote(types.VoteTypePrevote, nil, types.PartSetHeader{})
		return
	}

	// Valdiate proposal block
	err := cs.state.ValidateBlock(cs.ProposalBlock)
	if err != nil {
		// ProposalBlock is invalid, prevote nil.
		logger.Warn("enterPrevote: ProposalBlock is invalid", " error:", err)
		cs.signAddVote(types.VoteTypePrevote, nil, types.PartSetHeader{})
		return
	}

	// Valdiate proposal block
	proposedNextEpoch := ep.FromBytes(cs.ProposalBlock.ExData.BlockExData)
	if proposedNextEpoch != nil {
		err = cs.RoundState.Epoch.ValidateNextEpoch(proposedNextEpoch, height)
		if err != nil {
			// ProposalBlock is invalid, prevote nil.
			logger.Warn("enterPrevote: Proposal reward scheme is invalid", "error", err)
			cs.signAddVote(types.VoteTypePrevote, nil, types.PartSetHeader{})
			return
		}
	}

	// Prevote cs.ProposalBlock
	// NOTE: the proposal signature is validated when it is received,
	// and the proposal block parts are validated as they are received (against the merkle hash in the proposal)
	//cs.signAddVote(types.VoteTypePrevote, cs.ProposalBlock.Hash().Bytes(), cs.ProposalBlockParts.Header())
	cs.signAddVote(types.VoteTypePrevote, cs.Proposal.BlockHeaderHash().Bytes(), cs.Proposal.BlockPartsHeader)
	return
}

// Enter: any +2/3 prevotes at next round.
func (cs *ConsensusState) enterPrevoteWait(height int, round int) {
	if cs.Height != height || round < cs.Round || (cs.Round == round && RoundStepPrevoteWait <= cs.Step) {
		logger.Debug(Fmt("enterPrevoteWait(%v/%v): Invalid args. Current step: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))
		return
	}

	// Temp use here, need to change it to use cs.VoteSignAggr finally
	if !cs.Votes.Prevotes(round).HasTwoThirdsAny() {
		PanicSanity(Fmt("enterPrevoteWait(%v/%v), but Prevotes does not have any +2/3 votes", height, round))
	}
	logger.Info(Fmt("enterPrevoteWait(%v/%v). Current: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))

	defer func() {
		// Done enterPrevoteWait:
		cs.updateRoundStep(round, RoundStepPrevoteWait)
		cs.newStep()
	}()

	// Wait for some more prevotes; enterPrecommit
	cs.scheduleTimeout(cs.timeoutParams.Prevote(round), height, round, RoundStepPrevoteWait)
}

// Enter: +2/3 precomits for block or nil.
// Enter: `timeoutPrevote` after any +2/3 prevotes.
// Enter: any +2/3 precommits for next round.
// Lock & precommit the ProposalBlock if we have enough prevotes for it (a POL in this round)
// else, unlock an existing lock and precommit nil if +2/3 of prevotes were nil,
// else, precommit nil otherwise.
func (cs *ConsensusState) enterPrecommit(height int, round int) {
	logger.Error("enter precommit")
	if cs.Height != height || round < cs.Round || (cs.Round == round && RoundStepPrecommit <= cs.Step) {
		logger.Debug(Fmt("enterPrecommit(%v/%v): Invalid args. Current step: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))
		return
	}


	logger.Info(Fmt("enterPrecommit(%v/%v). Current: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))

	defer func() {
		// Done enterPrecommit:
		cs.updateRoundStep(round, RoundStepPrecommit)
		cs.newStep()
	}()

	blockID, ok := cs.VoteSignAggr.Prevotes(round).TwoThirdsMajority()
	fmt.Println(cs.VoteSignAggr.Prevotes(round).BlockID)

	// If we don't have a polka, we must precommit nil
	if !ok {
		if cs.LockedBlock != nil {
			logger.Info("enterPrecommit: No +2/3 prevotes during enterPrecommit while we're locked. Precommitting nil")
		} else {
			logger.Info("enterPrecommit: No +2/3 prevotes during enterPrecommit. Precommitting nil.")
		}
		cs.signAddVote(types.VoteTypePrecommit, nil, types.PartSetHeader{})
		return
	}


	// At this point +2/3 prevoted for a particular block or nil
	types.FireEventPolka(cs.evsw, cs.RoundStateEvent())

	// the latest POLRound should be this round
	polRound, _ := cs.VoteSignAggr.POLInfo()
	if polRound < round {
		PanicSanity(Fmt("This POLRound should be %v but got %", round, polRound))
	}

	// +2/3 prevoted nil. Unlock and precommit nil.
	if len(blockID.Hash) == 0 {
		if cs.LockedBlock == nil {
			logger.Info("enterPrecommit: +2/3 prevoted for nil.")
		} else {
			logger.Info("enterPrecommit: +2/3 prevoted for nil. Unlocking")
			cs.LockedRound = 0
			cs.LockedBlock = nil
			cs.LockedBlockParts = nil
			types.FireEventUnlock(cs.evsw, cs.RoundStateEvent())
		}
		cs.signAddVote(types.VoteTypePrecommit, nil, types.PartSetHeader{})
		return
	}

	// At this point, +2/3 prevoted for a particular block.

	// If we're already locked on that block, precommit it, and update the LockedRound
	if cs.LockedBlock.HashesTo(blockID.Hash) {
		logger.Info("enterPrecommit: +2/3 prevoted locked block. Relocking")
		cs.LockedRound = round
		types.FireEventRelock(cs.evsw, cs.RoundStateEvent())
		cs.signAddVote(types.VoteTypePrecommit, blockID.Hash.Bytes(), blockID.PartsHeader)
		return
	}

	// If +2/3 prevoted for proposal block, stage and precommit it
	fmt.Println(common.ToHex(cs.ProposalBlock.Hash().Bytes()))
	fmt.Println(common.ToHex(blockID.Hash.Bytes()))
	fmt.Println(blockID)
	if cs.ProposalBlock.HashesTo(blockID.Hash) {
		logger.Info("enterPrecommit: +2/3 prevoted proposal block. Locking", " hash:", blockID.Hash.Bytes())
		// Validate the block.
		if err := cs.state.ValidateBlock(cs.ProposalBlock); err != nil {
			PanicConsensus(Fmt("enterPrecommit: +2/3 prevoted for an invalid block: %v", err))
		}
		cs.LockedRound = round
		cs.LockedBlock = cs.ProposalBlock
		cs.LockedBlockParts = cs.ProposalBlockParts
		types.FireEventLock(cs.evsw, cs.RoundStateEvent())
		cs.signAddVote(types.VoteTypePrecommit, blockID.Hash.Bytes(), blockID.PartsHeader)
		return
	}

	// There was a polka in this round for a block we don't have.
	// Fetch that block, unlock, and precommit nil.
	// The +2/3 prevotes for this round is the POL for our unlock.
	// TODO: In the future save the POL prevotes for justification.
	cs.LockedRound = 0
	cs.LockedBlock = nil
	cs.LockedBlockParts = nil
	if !cs.ProposalBlockParts.HasHeader(blockID.PartsHeader) {
		cs.ProposalBlock = nil
		cs.ProposalBlockParts = types.NewPartSetFromHeader(blockID.PartsHeader)
	}
	types.FireEventUnlock(cs.evsw, cs.RoundStateEvent())
	cs.signAddVote(types.VoteTypePrecommit, nil, types.PartSetHeader{})
	return
}

// Enter: any +2/3 precommits for next round.
func (cs *ConsensusState) enterPrecommitWait(height int, round int) {
	if cs.Height != height || round < cs.Round || (cs.Round == round && RoundStepPrecommitWait <= cs.Step) {
		logger.Debug(Fmt("enterPrecommitWait(%v/%v): Invalid args. Current step: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))
		return
	}

	// Temp use here, need to change it to use cs.VoteSignAggr finally
	if !cs.Votes.Precommits(round).HasTwoThirdsAny() {
		PanicSanity(Fmt("enterPrecommitWait(%v/%v), but Precommits does not have any +2/3 votes", height, round))
	}
	logger.Info(Fmt("enterPrecommitWait(%v/%v). Current: %v/%v/%v", height, round, cs.Height, cs.Round, cs.Step))

	defer func() {
		// Done enterPrecommitWait:
		cs.updateRoundStep(round, RoundStepPrecommitWait)
		cs.newStep()
	}()

	// Wait for some more precommits; enterNewRound
	cs.scheduleTimeout(cs.timeoutParams.Precommit(round), height, round, RoundStepPrecommitWait)

}

// Enter: +2/3 precommits for block
func (cs *ConsensusState) enterCommit(height int, commitRound int) {
	if cs.Height != height || RoundStepCommit <= cs.Step {
		logger.Debug(Fmt("enterCommit(%v/%v): Invalid args. Current step: %v/%v/%v", height, commitRound, cs.Height, cs.Round, cs.Step))
		return
	}
	logger.Info(Fmt("enterCommit(%v/%v). Current: %v/%v/%v", height, commitRound, cs.Height, cs.Round, cs.Step))

	defer func() {
		// Done enterCommit:
		// keep cs.Round the same, commitRound points to the right Precommits set.
		cs.updateRoundStep(cs.Round, RoundStepCommit)
		cs.CommitRound = commitRound
		cs.CommitTime = time.Now()
		cs.newStep()

		// Maybe finalize immediately.
		cs.tryFinalizeCommit(height)

	}()

	blockID, ok := cs.VoteSignAggr.Precommits(commitRound).TwoThirdsMajority()
	if !ok {
		PanicSanity("RunActionCommit() expects +2/3 precommits")
	}

	// The Locked* fields no longer matter.
	// Move them over to ProposalBlock if they match the commit hash,
	// otherwise they'll be cleared in updateToState.
	if cs.LockedBlock.HashesTo(blockID.Hash) {
		cs.ProposalBlock = cs.LockedBlock
		cs.ProposalBlockParts = cs.LockedBlockParts
	}

	// If we don't have the block being committed, set up to get it.
	if !cs.ProposalBlock.HashesTo(blockID.Hash) {
		if !cs.ProposalBlockParts.HasHeader(blockID.PartsHeader) {
			// We're getting the wrong block.
			// Set up ProposalBlockParts and keep waiting.
			cs.ProposalBlock = nil
			cs.ProposalBlockParts = types.NewPartSetFromHeader(blockID.PartsHeader)
		} else {
			// We just need to keep waiting.
		}
	}
}

// If we have the block AND +2/3 commits for it, finalize.
func (cs *ConsensusState) tryFinalizeCommit(height int) {
	if cs.Height != height {
		PanicSanity(Fmt("tryFinalizeCommit() cs.Height: %v vs height: %v", cs.Height, height))
	}

	blockID, ok := cs.VoteSignAggr.Precommits(cs.CommitRound).TwoThirdsMajority()
	if !ok || len(blockID.Hash) == 0 {
		logger.Warn("Attempt to finalize failed. There was no +2/3 majority, or +2/3 was for <nil>.", " height:", height)
		return
	}
	if !cs.ProposalBlock.HashesTo(blockID.Hash) {
		// TODO: this happens every time if we're not a validator (ugly logs)
		// TODO: ^^ wait, why does it matter that we're a validator?
		logger.Warn("Attempt to finalize failed. We don't have the commit block.", " height:", height, "proposal-block", cs.ProposalBlock.Hash(), "commit-block", blockID.Hash)
		return
	}
	//	go
	cs.finalizeCommit(height)
}

// Increment height and goto RoundStepNewHeight
func (cs *ConsensusState) finalizeCommit(height int) {
	if cs.Height != height || cs.Step != RoundStepCommit {
		logger.Debug(Fmt("finalizeCommit(%v): Invalid args. Current step: %v/%v/%v", height, cs.Height, cs.Round, cs.Step))
		return
	}

	logger.Info("finalizeCommit: beginning", "cur height", cs.Height, "cur round", cs.Round)

	// fmt.Println("precommits:", cs.VoteSignAggr.Precommits(cs.CommitRound))
	blockID, ok := cs.VoteSignAggr.Precommits(cs.CommitRound).TwoThirdsMajority()
	block, blockParts := cs.ProposalBlock, cs.ProposalBlockParts

	if !ok {
		PanicSanity(Fmt("Cannot finalizeCommit, commit does not have two thirds majority"))
	}
	if !blockParts.HasHeader(blockID.PartsHeader) {
		PanicSanity(Fmt("Expected ProposalBlockParts header to be commit header"))
	}
	if !block.HashesTo(blockID.Hash) {
		PanicSanity(Fmt("Cannot finalizeCommit, ProposalBlock does not hash to commit hash"))
	}
	if err := cs.state.ValidateBlock(block); err != nil {
		PanicConsensus(Fmt("+2/3 committed an invalid block: %v", err))
	}

	logger.Info(Fmt("Finalizing commit of block with %d txs", block.NumTxs),
		"height", block.Height, "hash", block.Hash(), "root", block.AppHash)
	logger.Info(Fmt("%v", block))

	logger.Info("finalizeCommit: Wait for 15 minues before new height", "cur height", cs.Height, "cur round", cs.Round)
	//logger.Info(Fmt("finalizeCommit: cs.State: %#v\n", cs.GetRoundState()))
	time.Sleep(newHeightChangeSleepDuration)

	fail.Fail() // XXX

	// Save to blockStore.
	if cs.blockStore.Height() < block.Height {
		// NOTE: the seenCommit is local justification to commit this block,
		// but may differ from the LastCommit included in the next block
		precommits := cs.VoteSignAggr.Precommits(cs.CommitRound)

		seenCommit := precommits.MakeCommit()

		// Make emptry precommits now, may be replaced with signature aggregation
		//seenCommit := &types.Commit{}

		cs.blockStore.SaveBlock(block, blockParts, seenCommit)
	} else {
		// Happens during replay if we already saved the block but didn't commit
		logger.Info("Calling finalizeCommit on already stored block", " height:", block.Height)
	}

	fail.Fail() // XXX

	// Finish writing to the WAL for this height.
	// NOTE: If we fail before writing this, we'll never write it,
	// and just recover by running ApplyBlock in the Handshake.
	// If we moved it before persisting the block, we'd have to allow
	// WAL replay for blocks with an #ENDHEIGHT
	// As is, ConsensusState should not be started again
	// until we successfully call ApplyBlock (ie. here or in Handshake after restart)
	if cs.wal != nil {
		cs.wal.writeEndHeight(height)
	}

	fail.Fail() // XXX

	// Create a copy of the state for staging
	// and an event cache for txs
	stateCopy := cs.state.Copy()
	eventCache := types.NewEventCache(cs.evsw)

	// epochCopy := cs.epoch.Copy()
	// Execute and commit the block, update and save the state, and update the mempool.
	// All calls to the proxyAppConn come here.
	// NOTE: the block.AppHash wont reflect these txs until the next block
	err := stateCopy.ApplyBlock(eventCache, cs.proxyAppConn, block, blockParts.Header(), cs.mempool)
	if err != nil {
		logger.Error("Error on ApplyBlock. Did the application crash? Please restart tendermint", " error:", err)
		return
	}

	fail.Fail() // XXX

	// Fire event for new block.
	// NOTE: If we fail before firing, these events will never fire
	//
	// TODO: Either
	// 	* Fire before persisting state, in ApplyBlock
	//	* Fire on start up if we haven't written any new WAL msgs
	//   Both options mean we may fire more than once. Is that fine ?
	types.FireEventNewBlock(cs.evsw, types.EventDataNewBlock{block})
	types.FireEventNewBlockHeader(cs.evsw, types.EventDataNewBlockHeader{block.Header})
	eventCache.Flush()

	fail.Fail() // XXX

	// NewHeightStep!
	cs.updateToStateAndEpoch(stateCopy, stateCopy.Epoch)

	fail.Fail() // XXX

	// cs.StartTime is already set.
	// Schedule Round0 to start soon.
	cs.scheduleRound0(&cs.RoundState)

	// By here,
	// * cs.Height has been increment to height+1
	// * cs.Step is now RoundStepNewHeight
	// * cs.StartTime is set to when we will start round0.
	return
}

//-----------------------------------------------------------------------------
func (cs *ConsensusState) newSetProposal(proposal *types.Proposal) error {
	// Already have one
	// TODO: possibly catch double proposals

	if cs.Proposal != nil && proposal != nil{
		// TODO: if there are two proposals from the same proposer at one height, the propser will lose it's token
		return nil

	}
	if cs.Proposal != nil {
		return nil
	}

	// Does not apply
	if proposal.Height != cs.Height || proposal.Round != cs.Round {
		return nil
	}

	// We don't care about the proposal if we're already in RoundStepCommit.
	if RoundStepCommit <= cs.Step {
		return nil
	}

	// Verify POLRound, which must be -1 or between 0 and proposal.Round exclusive.
	if proposal.POLRound != -1 &&
		(proposal.POLRound < 0 || proposal.Round <= proposal.POLRound) {
		return ErrInvalidProposalPOLRound
	}

	// Verify signature
	if !cs.Validators.GetProposer().PubKey.VerifyBytes(types.SignBytes(cs.state.ChainID, proposal), proposal.Signature) {
		return ErrInvalidProposalSignature
	}

	cs.Proposal = proposal
	cs.ProposalBlockParts = types.NewPartSetFromHeader(proposal.BlockPartsHeader)
	cs.ProposerNetAddr = proposal.ProposerNetAddr
	cs.ProposerPeerKey = proposal.ProposerPeerKey
	if cs.ProposalBlockParts == nil {
		logger.Error("proposal block parts is nil")
	}
	return nil
}
func (cs *ConsensusState) defaultSetProposal(proposal *types.Proposal) error {
	// Already have one
	// TODO: possibly catch double proposals
	if cs.Proposal != nil {
		return nil
	}

	// Does not apply
	if proposal.Height != cs.Height || proposal.Round != cs.Round {
		return nil
	}

	// We don't care about the proposal if we're already in RoundStepCommit.
	if RoundStepCommit <= cs.Step {
		return nil
	}

	// Verify POLRound, which must be -1 or between 0 and proposal.Round exclusive.
	if proposal.POLRound != -1 &&
		(proposal.POLRound < 0 || proposal.Round <= proposal.POLRound) {
		return ErrInvalidProposalPOLRound
	}

	// Verify signature
	if !cs.Validators.GetProposer().PubKey.VerifyBytes(types.SignBytes(cs.state.ChainID, proposal), proposal.Signature) {
		return ErrInvalidProposalSignature
	}

	cs.Proposal = proposal
	cs.ProposalBlockParts = types.NewPartSetFromHeader(proposal.BlockPartsHeader)
	cs.ProposerPeerKey = proposal.ProposerPeerKey
	return nil
}

// NOTE: block is not necessarily valid.
// Asynchronously triggers either enterPrevote (before we timeout of propose) or tryFinalizeCommit, once we have the full block.
func (cs *ConsensusState) addProposalBlockPart(height int, part *types.Part, verify bool) (added bool, err error) {
	// Blocks might be reused, so round mismatch is OK
	if cs.Height != height {
		return false, nil
	}

	// We're not expecting a block part.
	if cs.ProposalBlockParts == nil {
		logger.Error("proposalBlockParts is nil")
		return false, nil // TODO: bad peer? Return error?
	}

	added, err = cs.ProposalBlockParts.AddPart(part, verify)
	if err != nil {
		return added, err
	}
	if added && cs.ProposalBlockParts.IsComplete() {
		// Added and completed!
		var n int
		var err error

		cs.ProposalBlock = wire.ReadBinary(&types.Block{}, cs.ProposalBlockParts.GetReader(), types.MaxBlockSize, &n, &err).(*types.Block)
		logger.Error("completed block:%v", cs.ProposalBlock)
		if !cs.isProposalComplete() {
			logger.Error("proposal is not completed")
		}
		// NOTE: it's possible to receive complete proposal blocks for future rounds without having the proposal
		logger.Info("Received complete proposal block", " height:", cs.ProposalBlock.Height, " hash:", cs.ProposalBlock.Hash())
		fmt.Printf("Received complete proposal block is %v\n", cs.ProposalBlock.String())
		fmt.Printf("block.LastCommit is %v\n", cs.ProposalBlock.LastCommit)
		fmt.Printf("Current cs.Step %v\n", cs.Step)
		//if cs.Step == RoundStepPropose && cs.isProposalComplete() {
		//		//		//	// Move onto the next step
		//		//		//	cs.enterPrevote(height, cs.Round)
		//		//		//} else
		//		//if cs.Step == RoundStepCommit {
		//		//	// If we're waiting on the proposal block...
		//		//	cs.tryFinalizeCommit(height)
		//		//}
		return true, err
	} else {
		logger.Error("block part is not completed")
	}
	return added, nil
}

// -----------------------------------------------------------------------------
func (cs *ConsensusState) setMaj23SignAggr(signAggr *types.SignAggr) (error, bool) {
	logger.Debug("enter setMaj23SignAggr()")
	logger.Debug("Received SignAggr %#v\n", signAggr)

	// Does not apply
	if signAggr.Height != cs.Height || signAggr.Round != cs.Round {
		logger.Error("does not apply")
		return nil, false
	}

	if signAggr.SignAggr() == nil {
		logger.Debug("SignAggr() is nil ")
	}
	maj23, err := cs.verifyMaj23SignAggr(signAggr)

	if err != nil || maj23 == false {
		logger.Info(Fmt("verifyMaj23SignAggr: Invalid signature aggregation for prevotes\n"))
		return ErrInvalidSignatureAggr, false
	}

	if signAggr.Type == types.VoteTypePrevote {
		// How if the signagure aggregation is for another block
		if cs.PrevoteMaj23SignAggr != nil {
			return ErrDuplicateSignatureAggr, false
		}

		cs.VoteSignAggr.AddSignAggr(signAggr)
		cs.PrevoteMaj23SignAggr = signAggr

		logger.Debug("setMaj23SignAggr:prevote aggr %#v\n", cs.PrevoteMaj23SignAggr)
	} else if signAggr.Type == types.VoteTypePrecommit {
		if cs.PrecommitMaj23SignAggr != nil {
			return ErrDuplicateSignatureAggr, false
		}

		cs.VoteSignAggr.AddSignAggr(signAggr)
		cs.PrecommitMaj23SignAggr = signAggr

		logger.Debug("setMaj23SignAggr:precommit aggr %#v\n", cs.PrecommitMaj23SignAggr)
	} else {
		logger.Warn(Fmt("setMaj23SignAggr: invalid type %d for signAggr %#v\n", signAggr.Type, signAggr))
		return ErrInvalidSignatureAggr, false
	}

	if signAggr.Type == types.VoteTypePrevote {
		logger.Info(Fmt("setMaj23SignAggr: Received 2/3+ prevotes for block %d, enter precommit\n", cs.Height))
		if cs.isProposalComplete() {
			logger.Error("receive block", cs.ProposalBlock)
			cs.enterPrecommit(cs.Height, cs.Round)
			return nil, true

		} else {
			logger.Error("block is not completed")
			return nil, false
		}


	} else if signAggr.Type == types.VoteTypePrecommit {
		logger.Info(Fmt("setMaj23SignAggr: Received 2/3+ precommits for block %d, enter commit\n", cs.Height))

		// TODO : Shall go to this state?
		// cs.tryFinalizeCommit(height)
		if cs.isProposalComplete() {
			logger.Error("block is not complete")

			cs.enterCommit(cs.Height, cs.Round)
			return nil, true
		} else {
			logger.Error("block is not completed")
			return nil, false
		}

	} else {
		panic("Invalid signAggr type")
		return nil, false
	}
	return nil, false
}

func (cs *ConsensusState) verifyMaj23SignAggr(signAggr *types.SignAggr) (bool, error) {
	logger.Info("enter verifyMaj23SignAggr()\n")

	// Assume BLSVerifySignAggr() will do following things
	// 1. Aggregate pub keys based on signAggr->BitArray
	// 2. Verify signature aggrefation is correct
	// 3. Verify +2/3 voting power exceeded

	maj23, err := cs.BLSVerifySignAggr(signAggr)

	if err != nil {
		logger.Debug("verifyMaj23SignAggr return with error \n")
		return false, err
	}

	return maj23, err
}

func (cs *ConsensusState) BLSVerifySignAggr(signAggr *types.SignAggr) (bool, error) {
	logger.Debug("enter BLSVerifySignAggr()\n")
	if signAggr == nil {
		return false, fmt.Errorf("Invalid SignAggr(nil)")
	}

	if signAggr.SignAggr() == nil {
		return false, fmt.Errorf("Invalid BLSSignature(nil)")
	}
	bitMap := signAggr.BitArray
	validators := cs.Validators
	quorum := cs.Validators.TotalVotingPower()*2/3 + 1
	if validators.Size()!= bitMap.Size() {
		return false, fmt.Errorf(Fmt("validators are not matched, consensus validators:%v, signAggr validators:%v"), validators.Validators, signAggr.BitArray)
	}

	powerSum, err := validators.TalliedVotingPower(bitMap)
	if err != nil {
		return false, err
	}

	aggrPubKey := validators.AggrPubKey(bitMap)
	if aggrPubKey == nil {
		return false, fmt.Errorf("can not aggregate pubkeys")
	}

	vote := &types.Vote{
		BlockID:          signAggr.BlockID,
		Height: signAggr.Height,
		Round: signAggr.Round,
		Type: signAggr.Type,
	}

	if !aggrPubKey.VerifyBytes(types.SignBytes(signAggr.ChainID, vote), (signAggr.SignAggr())) {
		return false, errors.New("Invalid aggregate signature")
	}

	var maj23 bool
	if powerSum >= quorum {
		maj23 = true
	} else {
		maj23 = false
	}
	return maj23,nil
}

// Attempt to add the vote. if its a duplicate signature, dupeout the validator
func (cs *ConsensusState) tryAddVote(vote *types.Vote, peerKey string) error {
	_, err := cs.addVote(vote, peerKey)
	if err != nil {
		// If the vote height is off, we'll just ignore it,
		// But if it's a conflicting sig, broadcast evidence tx for slashing.
		// If it's otherwise invalid, punish peer.
		if err == ErrVoteHeightMismatch {
			return err
		} else if _, ok := err.(*types.ErrVoteConflictingVotes); ok {
			if peerKey == "" {
				logger.Warn("Found conflicting vote from ourselves. Did you unsafe_reset a validator?", " height:", vote.Height, " round:", vote.Round, " type:", vote.Type)
				return err
			}
			logger.Warn("Found conflicting vote. Publish evidence (TODO)")
			/* TODO
			evidenceTx := &types.DupeoutTx{
				Address: address,
				VoteA:   *errDupe.VoteA,
				VoteB:   *errDupe.VoteB,
			}
			cs.mempool.BroadcastTx(struct{???}{evidenceTx}) // shouldn't need to check returned err
			*/
			return err
		} else {
			// Probably an invalid signature. Bad peer.
			logger.Warn("Error attempting to add vote", " error:", err)
			return ErrAddingVote
		}
	}
	return nil
}

//-----------------------------------------------------------------------------

func (cs *ConsensusState) addVote(vote *types.Vote, peerKey string) (added bool, err error) {
	logger.Debug("addVote", "voteHeight", vote.Height, "voteType", vote.Type, "csHeight", cs.Height)

	logger.Debug(Fmt("addVote: add vote %s\n", vote.String()))
/*
	// A precommit for the previous height?
	// These come in while we wait timeoutCommit
	if vote.Height+1 == cs.Height {
		if !(cs.Step == RoundStepNewHeight && vote.Type == types.VoteTypePrecommit) {
			// TODO: give the reason ..
			// fmt.Errorf("tryAddVote: Wrong height, not a LastCommit straggler commit.")
			return added, ErrVoteHeightMismatch
		}
		added, err = cs.LastCommit.AddVote(vote)
		if added {
			logger.Info(Fmt("Added to lastPrecommits: %v", cs.LastCommit.StringShort()))
			types.FireEventVote(cs.evsw, types.EventDataVote{vote})

			// if we can skip timeoutCommit and have all the votes now,
			if cs.timeoutParams.SkipTimeoutCommit && cs.LastCommit.HasAll() {
				// go straight to new round (skip timeout commit)
				// cs.scheduleTimeout(time.Duration(0), cs.Height, 0, RoundStepNewHeight)
				cs.enterNewRound(cs.Height, 0)
			}
		}

		return
	}
*/

	// A prevote/precommit for this height?
	if vote.Height == cs.Height {
		added, err = cs.Votes.AddVote(vote, peerKey)
		if added {
			if vote.Type == types.VoteTypePrevote {
				// If 2/3+ votes received, send them to other validators
				if cs.Votes.Prevotes(cs.Round).HasTwoThirdsMajority() {
					logger.Debug(Fmt("addVote: Got 2/3+ prevotes %+v\n", cs.Votes.Prevotes(cs.Round)))
					// Send votes aggregation
					//cs.sendMaj23Vote(vote.Type)

					// Send signature aggregation
					cs.sendMaj23SignAggr(vote.Type)
				}
			} else if vote.Type == types.VoteTypePrecommit {
				if cs.Votes.Precommits(cs.Round).HasTwoThirdsMajority() {
					logger.Debug(Fmt("addVote: Got 2/3+ precommits %+v\n", cs.Votes.Prevotes(cs.Round)))
					// Send votes aggregation
					//cs.sendMaj23Vote(vote.Type)

					// Send signature aggregation
					cs.sendMaj23SignAggr(vote.Type)
				}
			}

/*
			types.FireEventVote(cs.evsw, types.EventDataVote{vote})

			switch vote.Type {
			case types.VoteTypePrevote:
				prevotes := cs.Votes.Prevotes(vote.Round)
				logger.Info("Added to prevote", " vote:", vote, " prevotes:", prevotes.StringShort())
				// First, unlock if prevotes is a valid POL.
				// >> lockRound < POLRound <= unlockOrChangeLockRound (see spec)
				// NOTE: If (lockRound < POLRound) but !(POLRound <= unlockOrChangeLockRound),
				// we'll still enterNewRound(H,vote.R) and enterPrecommit(H,vote.R) to process it
				// there.
				if (cs.LockedBlock != nil) && (cs.LockedRound < vote.Round) && (vote.Round <= cs.Round) {
					blockID, ok := prevotes.TwoThirdsMajority()
					if ok && !cs.LockedBlock.HashesTo(blockID.Hash) {
						logger.Info("Unlocking because of POL.", " lockedRound:", cs.LockedRound, " POLRound:", vote.Round)
						cs.LockedRound = 0
						cs.LockedBlock = nil
						cs.LockedBlockParts = nil
						types.FireEventUnlock(cs.evsw, cs.RoundStateEvent())
					}
				}
				if cs.Round <= vote.Round && prevotes.HasTwoThirdsAny() {
					// Round-skip over to PrevoteWait or goto Precommit.
					cs.enterNewRound(height, vote.Round) // if the vote is ahead of us
					if prevotes.HasTwoThirdsMajority() {
						cs.enterPrecommit(height, vote.Round)
					} else {
						cs.enterPrevote(height, vote.Round) // if the vote is ahead of us
						cs.enterPrevoteWait(height, vote.Round)
					}
				} else if cs.Proposal != nil && 0 <= cs.Proposal.POLRound && cs.Proposal.POLRound == vote.Round {
					// If the proposal is now complete, enter prevote of cs.Round.
					if cs.isProposalComplete() {
						cs.enterPrevote(height, cs.Round)
					}
				}
			case types.VoteTypePrecommit:
				precommits := cs.Votes.Precommits(vote.Round)
				logger.Info("Added to precommit", " vote:", vote, " precommits:", precommits.StringShort())
				blockID, ok := precommits.TwoThirdsMajority()
				if ok {
					if len(blockID.Hash) == 0 {
						cs.enterNewRound(height, vote.Round+1)
					} else {
						cs.enterNewRound(height, vote.Round)
						cs.enterPrecommit(height, vote.Round)
						cs.enterCommit(height, vote.Round)

						if cs.timeoutParams.SkipTimeoutCommit && precommits.HasAll() {
							// if we have all the votes now,
							// go straight to new round (skip timeout commit)
							// cs.scheduleTimeout(time.Duration(0), cs.Height, 0, RoundStepNewHeight)
							cs.enterNewRound(cs.Height, 0)
						}

					}
				} else if cs.Round <= vote.Round && precommits.HasTwoThirdsAny() {
					cs.enterNewRound(height, vote.Round)
					cs.enterPrecommit(height, vote.Round)
					cs.enterPrecommitWait(height, vote.Round)
				}
			default:
				PanicSanity(Fmt("Unexpected vote type %X", vote.Type)) // Should not happen.
			}
*/
		}

		// Either duplicate, or error upon cs.Votes.AddByIndex()
		return
	} else {
		err = ErrVoteHeightMismatch
	}

	// Height mismatch, bad peer?
	logger.Info("Vote ignored and not added", "voteHeight", vote.Height, "csHeight", cs.Height, "err", err)
	return
}

func (cs *ConsensusState) signVote(type_ byte, hash []byte, header types.PartSetHeader) (*types.Vote, error) {
	addr := cs.privValidator.GetAddress()
	valIndex, _ := cs.Validators.GetByAddress(addr)
	vote := &types.Vote{
		ValidatorAddress: types.BytesToHash160(addr),
		ValidatorIndex:   valIndex,
		Height:           cs.Height,
		Round:            cs.Round,
		Type:             type_,
		BlockID:          types.BlockID{types.BytesToHash160(hash), header},
	}
	err := cs.privValidator.SignVote(cs.state.ChainID, vote)
	return vote, err
}

// sign the vote and publish on internalMsgQueue
func (cs *ConsensusState) signAddVote(type_ byte, hash []byte, header types.PartSetHeader) *types.Vote {
	// if we don't have a key or we're not in the validator set, do nothing
	if cs.privValidator == nil || !cs.Validators.HasAddress(cs.privValidator.GetAddress()) {
		return nil
	}
	vote, err := cs.signVote(type_, hash, header)
	if err == nil {
		logger.Error(Fmt("vote:%v",common.ToHex(vote.Signature.Bytes())))
		logger.Error("chainID:", vote.BlockID)
		pub :=cs.privValidator.GetPubKey()
		if !pub.VerifyBytes(types.SignBytes(cs.state.ChainID, vote), vote.Signature) {
			logger.Error("verify signature failed:")
		} else {
			logger.Error("verify signature succ")
		}
		cs.sendInternalMessage(msgInfo{&VoteMessage{vote}, ""})
		if !cs.IsProposer() && cs.ProposerPeerKey != "" {
			v2pMsg := types.EventDataVote2Proposer{vote, cs.ProposerPeerKey}
			types.FireEventVote2Proposer(cs.evsw, v2pMsg)
		}
		logger.Info("Signed and pushed vote", " height:", cs.Height, " round:", cs.Round, " vote:", vote, " error:", err)
		return vote
	} else {
		//if !cs.replayMode {
		logger.Warn("Error signing vote", " height:", cs.Height, " round:", cs.Round, " vote:", vote, " error:", err)
		//}
		return nil
	}
}

// Build the 2/3+ signature aggregation based on vote set and send it to other validators
func (cs *ConsensusState) sendMaj23SignAggr(voteType byte) {
	logger.Info("Enter sendMaj23SignAggr()")

	var votes []*types.Vote
	var blockID, maj23 types.BlockID
	var ok bool

	if voteType == types.VoteTypePrevote {
		votes = cs.Votes.Prevotes(cs.Round).Votes()
		maj23, ok = cs.Votes.Prevotes(cs.Round).TwoThirdsMajority()
	} else if voteType == types.VoteTypePrecommit {
		votes = cs.Votes.Precommits(cs.Round).Votes()
		maj23, ok = cs.Votes.Prevotes(cs.Round).TwoThirdsMajority()
	}

	if ok == false {
		logger.Fatal("Votset does not have +2/3 voting")
	}

	numValidators := cs.Validators.Size()
	signBitArray := NewBitArray(numValidators)
	var sigs []*crypto.Signature
	var ss []byte
	fmt.Println(ss)
	for index, vote := range votes {
		if vote != nil {
			blockID = vote.BlockID
			ss = vote.SignBytes
			signBitArray.SetIndex(index, true)
			sigs = append(sigs, &(vote.Signature))
		}
	}

	// step 1: build BLS signature aggregation based on signatures in votes
	// bitarray, signAggr := BuildSignAggr(votes)
	signature := crypto.BLSSignatureAggregate(sigs)
	if signature == nil {
		logger.Fatal("Can not aggregate signature")
		return
	}

	signAggr := types.MakeSignAggr(cs.Height, cs.Round, voteType, numValidators, blockID, cs.Votes.chainID, signBitArray, signature)
	signAggr.SignBytes = ss

	// Set sign bitmap
	//signAggr.SetBitArray(signBitArray)

	if maj23.IsZero() == true {
		logger.Debug("The maj23 blockID is zero %#v\n", maj23)
		panic("Invalid maj23")
	}

	// Set ma23 block ID
	signAggr.SetMaj23(maj23)
	logger.Debug(Fmt("Generate Maj23SignAggr %#v\n", signAggr))

	signEvent := types.EventDataSignAggr{SignAggr:signAggr}
	types.FireEventSignAggr(cs.evsw, signEvent)

	// send sign aggregate msg on internal msg queue
	cs.sendInternalMessage(msgInfo{&Maj23SignAggrMessage{signAggr}, ""})
}

//---------------------------------------------------------

func CompareHRS(h1, r1 int, s1 RoundStepType, h2, r2 int, s2 RoundStepType) int {
	if h1 < h2 {
		return -1
	} else if h1 > h2 {
		return 1
	}
	if r1 < r2 {
		return -1
	} else if r1 > r2 {
		return 1
	}
	if s1 < s2 {
		return -1
	} else if s1 > s2 {
		return 1
	}
	return 0
}
