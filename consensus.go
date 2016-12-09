package ipfscluster

import (
	"context"
	"errors"
	"time"

	host "gx/ipfs/QmPTGbC34bPKaUm9wTxBo7zSCac7pDuG42ZmnXC718CKZZ/go-libp2p-host"
	consensus "gx/ipfs/QmZ88KbrvZMJpXaNwAGffswcYKz8EbeafzAFGMCA6MEZKt/go-libp2p-consensus"
	libp2praft "gx/ipfs/QmdHo2LQKmGQ6rDAWFxnzNuW3z8b6Xmw3wEFsMQaj9Rsqj/go-libp2p-raft"
	peer "gx/ipfs/QmfMmLGoKzCHDN7cGgk64PJr4iipzidDRME8HABSJqvmhC/go-libp2p-peer"

	cid "gx/ipfs/QmcTcsTvfaeEBRFo1TkFgT8sRmgi1n1LTZpecfVP8fzpGD/go-cid"
)

const (
	maxSnapshots   = 5
	raftSingleMode = true
)

// Type of pin operation
const (
	LogOpPin = iota + 1
	LogOpUnpin
)

type clusterLogOpType int

// We will wait for the consensus state to be updated up to this
// amount of seconds.
var MaxStartupDelay = 10 * time.Second

// clusterLogOp represents an operation for the OpLogConsensus system.
// It implements the consensus.Op interface.
type clusterLogOp struct {
	Cid   string
	Type  clusterLogOpType
	ctx   context.Context
	rpcCh chan ClusterRPC
}

// ApplyTo applies the operation to the ClusterState
func (op clusterLogOp) ApplyTo(cstate consensus.State) (consensus.State, error) {
	state, ok := cstate.(ClusterState)
	var err error
	if !ok {
		// Should never be here
		panic("Received unexpected state type")
	}

	c, err := cid.Decode(op.Cid)
	if err != nil {
		// Should never be here
		panic("Could not decode a CID we ourselves encoded")
	}

	ctx, cancel := context.WithCancel(op.ctx)
	defer cancel()

	switch op.Type {
	case LogOpPin:
		err := state.AddPin(c)
		if err != nil {
			goto ROLLBACK
		}
		// Async, we let the PinTracker take care of any problems
		MakeRPC(ctx, op.rpcCh, RPC(IPFSPinRPC, c), false)
	case LogOpUnpin:
		err := state.RmPin(c)
		if err != nil {
			goto ROLLBACK
		}
		// Async, we let the PinTracker take care of any problems
		MakeRPC(ctx, op.rpcCh, RPC(IPFSUnpinRPC, c), false)
	default:
		logger.Error("unknown clusterLogOp type. Ignoring")
	}
	return state, nil

ROLLBACK:
	// We failed to apply the operation to the state
	// and therefore we need to request a rollback to the
	// cluster to the previous state. This operation can only be performed
	// by the cluster leader.
	rllbckRPC := RPC(RollbackRPC, state)
	leadrRPC := RPC(LeaderRPC, rllbckRPC)
	MakeRPC(ctx, op.rpcCh, leadrRPC, false)
	logger.Errorf("an error ocurred when applying Op to state: %s", err)
	logger.Error("a rollback was requested")
	// Make sure the consensus algorithm nows this update did not work
	return nil, errors.New("a rollback was requested. Reason: " + err.Error())
}

// ClusterConsensus handles the work of keeping a shared-state between
// the members of an IPFS Cluster, as well as modifying that state and
// applying any updates in a thread-safe manner.
type ClusterConsensus struct {
	ctx context.Context

	consensus consensus.OpLogConsensus
	actor     consensus.Actor

	rpcCh chan ClusterRPC

	p2pRaft *libp2pRaftWrap
}

// NewClusterConsensus builds a new ClusterConsensus component. The state
// is used to initialize the Consensus system, so any information in it
// is discarded.
func NewClusterConsensus(cfg *ClusterConfig, host host.Host, state ClusterState) (*ClusterConsensus, error) {
	logger.Info("Starting Consensus component")
	ctx := context.Background()
	rpcCh := make(chan ClusterRPC, RPCMaxQueue)
	op := clusterLogOp{
		ctx:   ctx,
		rpcCh: rpcCh,
	}
	con, actor, wrapper, err := makeLibp2pRaft(cfg, host, state, op)
	if err != nil {
		return nil, err
	}

	con.SetActor(actor)

	cc := &ClusterConsensus{
		ctx:       ctx,
		consensus: con,
		actor:     actor,
		rpcCh:     rpcCh,
		p2pRaft:   wrapper,
	}

	logger.Info("Waiting for Consensus state to catch up")
	time.Sleep(1 * time.Second)
	start := time.Now()
	for {
		time.Sleep(500 * time.Millisecond)
		li := wrapper.raft.LastIndex()
		lai := wrapper.raft.AppliedIndex()
		if lai == li || time.Since(start) > MaxStartupDelay {
			break
		}
		logger.Debugf("Waiting for Raft index: %d/%d", lai, li)
	}

	return cc, nil
}

// Shutdown stops the component so it will not process any
// more updates. The underlying consensus is permanently
// shutdown, along with the libp2p transport.
func (cc *ClusterConsensus) Shutdown() error {
	logger.Info("Stopping Consensus component")

	// When we take snapshot, we make sure that
	// we re-start from the previous state, and that
	// we don't replay the log. This includes
	// pin and pin certain stuff.
	f := cc.p2pRaft.raft.Snapshot()
	_ = f.Error()
	f = cc.p2pRaft.raft.Shutdown()
	err := f.Error()
	cc.p2pRaft.transport.Close()
	if err != nil {
		return err
	}
	return nil
}

// RpcChan can be used by Cluster to read any
// requests from this component
func (cc *ClusterConsensus) RpcChan() <-chan ClusterRPC {
	return cc.rpcCh
}

func (cc *ClusterConsensus) op(c *cid.Cid, t clusterLogOpType) clusterLogOp {
	return clusterLogOp{
		Cid:  c.String(),
		Type: t,
	}
}

// AddPin submits a Cid to the shared state of the cluster.
func (cc *ClusterConsensus) AddPin(c *cid.Cid) error {
	// Create pin operation for the log
	op := cc.op(c, LogOpPin)
	_, err := cc.consensus.CommitOp(op)
	if err != nil {
		// This means the op did not make it to the log
		return err
	}
	logger.Infof("Pin commited to global state: %s", c)
	return nil
}

// RmPin removes a Cid from the shared state of the cluster.
func (cc *ClusterConsensus) RmPin(c *cid.Cid) error {
	// Create  unpin operation for the log
	op := cc.op(c, LogOpUnpin)
	_, err := cc.consensus.CommitOp(op)
	if err != nil {
		return err
	}
	logger.Infof("Unpin commited to global state: %s", c)
	return nil
}

func (cc *ClusterConsensus) State() (ClusterState, error) {
	st, err := cc.consensus.GetLogHead()
	if err != nil {
		return nil, err
	}
	state, ok := st.(ClusterState)
	if !ok {
		return nil, errors.New("Wrong state type")
	}
	return state, nil
}

// Leader() returns the peerID of the Leader of the
// cluster.
func (cc *ClusterConsensus) Leader() peer.ID {
	// FIXME: Hashicorp Raft specific
	raftactor := cc.actor.(*libp2praft.Actor)
	return raftactor.Leader()
}

func (cc *ClusterConsensus) Rollback(state ClusterState) error {
	return cc.consensus.Rollback(state)
}
