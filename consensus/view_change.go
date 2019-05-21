package consensus

import (
	"bytes"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/bls/ffi/go/bls"
	msg_pb "github.com/harmony-one/harmony/api/proto/message"
	bls_cosi "github.com/harmony-one/harmony/crypto/bls"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/p2p/host"
)

// PbftPhase  PBFT phases: pre-prepare, prepare and commit
type PbftPhase int

// Enum for PbftPhase
const (
	Announce PbftPhase = iota
	Prepare
	Commit
)

// Mode determines whether a node is in normal or viewchanging mode
type Mode int

// Enum for node Mode
const (
	Normal Mode = iota
	ViewChanging
)

// PbftMode contains mode and consensusID of viewchanging
type PbftMode struct {
	mode        Mode
	consensusID uint32
	mux         sync.Mutex
}

// Mode return the current node mode
func (pm *PbftMode) Mode() Mode {
	return pm.mode
}

// SetMode set the node mode as required
func (pm *PbftMode) SetMode(m Mode) {
	pm.mux.Lock()
	defer pm.mux.Unlock()
	pm.mode = m
}

// ConsensusID return the current viewchanging id
func (pm *PbftMode) ConsensusID() uint32 {
	return pm.consensusID
}

// SetConsensusID sets the viewchanging id accordingly
func (pm *PbftMode) SetConsensusID(consensusID uint32) {
	pm.mux.Lock()
	defer pm.mux.Unlock()
	pm.consensusID = consensusID
}

// GetConsensusID returns the current viewchange consensusID
func (pm *PbftMode) GetConsensusID() uint32 {
	return pm.consensusID
}

// switchPhase will switch PbftPhase to nextPhase if the desirePhase equals the nextPhase
func (consensus *Consensus) switchPhase(desirePhase PbftPhase) {
	utils.GetLogInstance().Debug("switchPhase: ", "desirePhase", desirePhase, "myPhase", consensus.phase)

	var nextPhase PbftPhase
	switch consensus.phase {
	case Announce:
		nextPhase = Prepare
	case Prepare:
		nextPhase = Commit
	case Commit:
		nextPhase = Announce
	}
	if nextPhase == desirePhase {
		consensus.phase = nextPhase
	}
}

// GetNextLeaderKey uniquely determine who is the leader for given consensusID
func (consensus *Consensus) GetNextLeaderKey() *bls.PublicKey {
	idx := consensus.getIndexOfPubKey(consensus.LeaderPubKey)
	if idx == -1 {
		utils.GetLogInstance().Warn("GetNextLeaderKey: currentLeaderKey not found", "key", consensus.LeaderPubKey.GetHexString())
	}
	idx = (idx + 1) % len(consensus.PublicKeys)
	return consensus.PublicKeys[idx]
}

func (consensus *Consensus) getIndexOfPubKey(pubKey *bls.PublicKey) int {
	for k, v := range consensus.PublicKeys {
		if v.IsEqual(pubKey) {
			return k
		}
	}
	return -1
}

// ResetViewChangeState reset the state for viewchange
func (consensus *Consensus) ResetViewChangeState() {
	consensus.mode.SetMode(Normal)
	bhpBitmap, _ := bls_cosi.NewMask(consensus.PublicKeys, nil)
	nilBitmap, _ := bls_cosi.NewMask(consensus.PublicKeys, nil)
	consensus.bhpBitmap = bhpBitmap
	consensus.nilBitmap = nilBitmap

	consensus.bhpSigs = map[common.Address]*bls.Sign{}
	consensus.nilSigs = map[common.Address]*bls.Sign{}
	consensus.aggregatedBHPSig = nil
	consensus.aggregatedNILSig = nil
}

func createTimeout() map[string]*utils.Timeout {
	timeouts := make(map[string]*utils.Timeout)
	strs := []string{"announce", "prepare", "commit"}
	for _, s := range strs {
		timeouts[s] = utils.NewTimeout(phaseDuration)
	}
	timeouts["bootstrap"] = utils.NewTimeout(bootstrapDuration)
	timeouts["viewchange"] = utils.NewTimeout(viewChangeDuration)
	return timeouts
}

// startViewChange send a  new view change
func (consensus *Consensus) startViewChange(consensusID uint32) {
	for k := range consensus.consensusTimeout {
		if k != "viewchange" {
			consensus.consensusTimeout[k].Stop()
		}
	}
	consensus.mode.SetMode(ViewChanging)
	consensus.mode.SetConsensusID(consensusID)
	nextLeaderKey := consensus.GetNextLeaderKey()
	consensus.LeaderPubKey = consensus.GetNextLeaderKey()
	if nextLeaderKey.IsEqual(consensus.PubKey) {
		return
	}

	diff := consensusID - consensus.consensusID
	duration := time.Duration(int64(diff) * int64(viewChangeDuration))
	utils.GetLogInstance().Info("startViewChange", "consensusID", consensusID, "timeoutDuration", duration, "nextLeader", consensus.LeaderPubKey.GetHexString()[:10])

	msgToSend := consensus.constructViewChangeMessage()
	consensus.host.SendMessageToGroups([]p2p.GroupID{p2p.NewGroupIDByShardID(p2p.ShardID(consensus.ShardID))}, host.ConstructP2pMessage(byte(17), msgToSend))

	consensus.consensusTimeout["viewchange"].SetDuration(duration)
	consensus.consensusTimeout["viewchange"].Start()

}

// new leader send new view message
func (consensus *Consensus) startNewView() {
	utils.GetLogInstance().Info("startNewView", "consensusID", consensus.mode.GetConsensusID())
	consensus.mode.SetMode(Normal)
	consensus.switchPhase(Announce)

	msgToSend := consensus.constructNewViewMessage()
	consensus.host.SendMessageToGroups([]p2p.GroupID{p2p.NewGroupIDByShardID(p2p.ShardID(consensus.ShardID))}, host.ConstructP2pMessage(byte(17), msgToSend))
}

func (consensus *Consensus) onViewChange(msg *msg_pb.Message) {
	senderKey, validatorAddress, err := consensus.verifyViewChangeSenderKey(msg)
	if err != nil {
		utils.GetLogInstance().Debug("onViewChange verifySenderKey failed", "error", err)
		return
	}

	recvMsg, err := ParseViewChangeMessage(msg)
	if err != nil {
		utils.GetLogInstance().Warn("onViewChange unable to parse viewchange message")
		return
	}
	newLeaderKey := recvMsg.LeaderPubkey
	if !consensus.PubKey.IsEqual(newLeaderKey) {
		return
	}

	utils.GetLogInstance().Warn("onViewChange received", "viewChangeID", recvMsg.ConsensusID, "myCurrentID", consensus.consensusID, "ValidatorAddress", consensus.SelfAddress)

	if consensus.seqNum > recvMsg.SeqNum {
		return
	}
	if consensus.mode.Mode() == ViewChanging && consensus.mode.GetConsensusID() > recvMsg.ConsensusID {
		return
	}
	if err = verifyMessageSig(senderKey, msg); err != nil {
		utils.GetLogInstance().Debug("onViewChange Failed to verify sender's signature", "error", err)
		return
	}

	consensus.vcLock.Lock()
	defer consensus.vcLock.Unlock()

	consensus.mode.SetMode(ViewChanging)
	consensus.mode.SetConsensusID(recvMsg.ConsensusID)

	_, ok1 := consensus.nilSigs[consensus.SelfAddress]
	_, ok2 := consensus.bhpSigs[consensus.SelfAddress]
	if !(ok1 || ok2) {
		// add own signature for newview message
		preparedMsgs := consensus.pbftLog.GetMessagesByTypeSeq(msg_pb.MessageType_PREPARED, recvMsg.SeqNum)
		if len(preparedMsgs) == 0 {
			sign := consensus.priKey.SignHash(NIL)
			consensus.nilSigs[consensus.SelfAddress] = sign
			consensus.nilBitmap.SetKey(consensus.PubKey, true)
		} else {
			if len(preparedMsgs) > 1 {
				utils.GetLogInstance().Debug("onViewChange more than 1 prepared message for new leader")
			}
			msgToSign := append(preparedMsgs[0].BlockHash[:], preparedMsgs[0].Payload...)
			consensus.bhpSigs[consensus.SelfAddress] = consensus.priKey.SignHash(msgToSign)
			consensus.bhpBitmap.SetKey(consensus.PubKey, true)
		}
	}

	if (len(consensus.bhpSigs) + len(consensus.nilSigs)) >= ((len(consensus.PublicKeys)*2)/3 + 1) {
		return
	}

	// m2 type message
	if len(recvMsg.Payload) == 0 {
		_, ok := consensus.nilSigs[validatorAddress]
		if ok {
			utils.GetLogInstance().Debug("onViewChange already received m2 message from the validator", "validatorAddress", validatorAddress)
			return
		}

		if !recvMsg.ViewchangeSig.VerifyHash(senderKey, NIL) {
			utils.GetLogInstance().Warn("onViewChange failed to verify signature for m2 type viewchange message")
			return
		}
		consensus.nilSigs[validatorAddress] = recvMsg.ViewchangeSig
		consensus.nilBitmap.SetKey(recvMsg.SenderPubkey, true) // Set the bitmap indicating that this validator signed.
	} else { // m1 type message
		_, ok := consensus.bhpSigs[validatorAddress]
		if ok {
			utils.GetLogInstance().Debug("onViewChange already received m1 message from the validator", "validatorAddress", validatorAddress)
			return
		}
		if !recvMsg.ViewchangeSig.VerifyHash(recvMsg.SenderPubkey, recvMsg.Payload) {
			utils.GetLogInstance().Warn("onViewChange failed to verify signature for m1 type viewchange message")
			return
		}
		// first time receive m1 type message, need verify validity of prepared message
		if len(consensus.m1Payload) == 0 {
			//#### Read payload data
			offset := 0
			blockHash := recvMsg.Payload[offset : offset+32]
			offset += 32
			// 48 byte of multi-sig
			multiSig := recvMsg.Payload[offset : offset+48]
			offset += 48
			// bitmap
			bitmap := recvMsg.Payload[offset:]
			//#### END Read payload data
			// Verify the multi-sig for prepare phase
			deserializedMultiSig := bls.Sign{}
			err = deserializedMultiSig.Deserialize(multiSig)
			if err != nil {
				utils.GetLogInstance().Warn("onViewChange failed to deserialize the multi signature for prepared payload", "error", err)
				return
			}
			mask, err := bls_cosi.NewMask(consensus.PublicKeys, nil)
			mask.SetMask(bitmap)
			// TODO: add 2f+1 signature checking
			if !deserializedMultiSig.VerifyHash(mask.AggregatePublic, blockHash[:]) || err != nil {
				utils.GetLogInstance().Warn("onViewChange failed to verify multi signature for m1 prepared payload", "error", err, "blockHash", blockHash)
				return
			}
			consensus.m1Payload = append(recvMsg.Payload[:0:0], recvMsg.Payload...)
		}
		// consensus.m1Payload already verified
		if !bytes.Equal(consensus.m1Payload, recvMsg.Payload) {
			utils.GetLogInstance().Warn("onViewChange m1 message payload not equal, hence invalid")
			return
		}
		consensus.bhpSigs[validatorAddress] = recvMsg.ViewchangeSig
		consensus.bhpBitmap.SetKey(recvMsg.SenderPubkey, true) // Set the bitmap indicating that this validator signed.
	}

	if (len(consensus.bhpSigs) + len(consensus.nilSigs)) >= ((len(consensus.PublicKeys)*2)/3 + 1) {
		consensus.mode.SetMode(Normal)
		consensus.LeaderPubKey = consensus.PubKey
		if len(consensus.m1Payload) == 0 {
			consensus.phase = Announce
			go func() {
				consensus.ReadySignal <- struct{}{}
			}()
		} else {
			consensus.phase = Commit
			copy(consensus.blockHash[:], consensus.m1Payload[:32])
			//#### Read payload data
			offset := 32
			// 48 byte of multi-sig
			multiSig := recvMsg.Payload[offset : offset+48]
			offset += 48
			// bitmap
			bitmap := recvMsg.Payload[offset:]
			//#### END Read payload data
			aggSig := bls.Sign{}
			_ = aggSig.Deserialize(multiSig)
			mask, _ := bls_cosi.NewMask(consensus.PublicKeys, nil)
			mask.SetMask(bitmap)
			consensus.aggregatedPrepareSig = &aggSig
			consensus.prepareBitmap = mask

			// Leader sign the multi-sig and bitmap (for commit phase)
			consensus.commitSigs[consensus.SelfAddress] = consensus.priKey.SignHash(consensus.m1Payload[32:])
		}

		msgToSend := consensus.constructNewViewMessage()

		utils.GetLogInstance().Warn("onViewChange", "sent newview message", len(msgToSend))
		consensus.host.SendMessageToGroups([]p2p.GroupID{p2p.NewGroupIDByShardID(p2p.ShardID(consensus.ShardID))}, host.ConstructP2pMessage(byte(17), msgToSend))

		consensus.consensusID = consensus.mode.GetConsensusID()
		consensus.ResetViewChangeState()
		consensus.ResetState()
		consensus.consensusTimeout["viewchange"].Stop()

	}
	utils.GetLogInstance().Debug("onViewChange", "numSigs", len(consensus.bhpSigs)+len(consensus.nilSigs), "needed", (len(consensus.PublicKeys)*2)/3+1)
}

// TODO: move to consensus_leader.go later
func (consensus *Consensus) onNewView(msg *msg_pb.Message) {
	utils.GetLogInstance().Info("onNewView received new view message")
	senderKey, _, err := consensus.verifyViewChangeSenderKey(msg)
	if err != nil {
		utils.GetLogInstance().Debug("onNewView verifySenderKey failed", "error", err)
		return
	}
	recvMsg, err := consensus.ParseNewViewMessage(msg)
	if err != nil {
		utils.GetLogInstance().Warn("onViewChange unable to parse viewchange message")
		return
	}

	if !consensus.LeaderPubKey.IsEqual(senderKey) {
		utils.GetLogInstance().Warn("onNewView key not match", "senderKey", senderKey.GetHexString()[:10], "newLeaderKey", consensus.LeaderPubKey.GetHexString()[:10])
		return
	}
	if consensus.seqNum > recvMsg.SeqNum {
		return
	}
	if err = verifyMessageSig(senderKey, msg); err != nil {
		utils.GetLogInstance().Debug("onNewView failed to verify new leader's signature", "error", err)
		return
	}

	consensus.vcLock.Lock()
	defer consensus.vcLock.Unlock()

	// TODO check total number of sigs > 2f+1
	if recvMsg.M1AggSig != nil {
		m1Sig := recvMsg.M1AggSig
		m1Mask := recvMsg.M1Bitmap
		consensus.aggregatedBHPSig = m1Sig
		consensus.bhpBitmap = m1Mask
		if !m1Sig.VerifyHash(m1Mask.AggregatePublic, recvMsg.Payload) {
			utils.GetLogInstance().Warn("onNewView unable to verify aggregated signature of m1 payload")
			return
		}
	}
	if recvMsg.M2AggSig != nil {
		m2Sig := recvMsg.M2AggSig
		m2Mask := recvMsg.M2Bitmap
		if !m2Sig.VerifyHash(m2Mask.AggregatePublic, NIL) {
			utils.GetLogInstance().Warn("onNewView unable to verify aggregated signature of m2 payload")
			return
		}
	}

	if len(recvMsg.Payload) > 0 && recvMsg.M1AggSig != nil {
		//#### Read payload data
		blockHash := recvMsg.Payload[:32]
		offset := 32
		// 48 byte of multi-sig
		multiSig := recvMsg.Payload[offset : offset+48]
		offset += 48
		// bitmap
		bitmap := recvMsg.Payload[offset:]
		//#### END Read payload data

		aggSig := bls.Sign{}
		err := aggSig.Deserialize(multiSig)
		if err != nil {
			utils.GetLogInstance().Warn("onNewView unable to deserialize prepared message agg sig", "err", err)
			return
		}
		mask, err := bls_cosi.NewMask(consensus.PublicKeys, nil)
		if err != nil {
			utils.GetLogInstance().Warn("onNewView unable to setup mask for prepared message", "err", err)
			return
		}
		mask.SetMask(bitmap)
		if !aggSig.VerifyHash(mask.AggregatePublic, blockHash) {
			utils.GetLogInstance().Warn("onNewView failed to verify signature for prepared message")
			return
		}
		copy(consensus.blockHash[:], blockHash)
		consensus.aggregatedPrepareSig = &aggSig
		consensus.prepareBitmap = mask

		//create prepared message?: consensus.pbftLog.AddMessage(recvMsg)

		if recvMsg.SeqNum > consensus.seqNum {
			return
		}

		// Construct and send the commit message
		multiSigAndBitmap := append(multiSig, bitmap...)
		msgToSend := consensus.constructCommitMessage(multiSigAndBitmap)
		utils.GetLogInstance().Info("onNewView === commit", "sent commit message", len(msgToSend))
		consensus.host.SendMessageToGroups([]p2p.GroupID{p2p.NewGroupIDByShardID(p2p.ShardID(consensus.ShardID))}, host.ConstructP2pMessage(byte(17), msgToSend))

		consensus.phase = Commit
		consensus.consensusTimeout["commit"].Start()
	} else {
		consensus.phase = Announce
		consensus.consensusTimeout["announce"].Start()
		utils.GetLogInstance().Info("onNewView === announce")
	}
	consensus.consensusID = consensus.mode.GetConsensusID()
	consensus.ResetViewChangeState()
	consensus.ResetState()
	consensus.consensusTimeout["viewchange"].Stop()

}