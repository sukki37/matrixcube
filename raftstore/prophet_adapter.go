package raftstore

import (
	"math"
	"time"

	etcdraftpb "github.com/coreos/etcd/raft/raftpb"
	"github.com/deepfabric/beehive/metric"
	"github.com/deepfabric/beehive/pb/metapb"
	"github.com/deepfabric/beehive/pb/raftcmdpb"
	"github.com/deepfabric/beehive/pb/raftpb"
	"github.com/deepfabric/beehive/util"
	"github.com/deepfabric/prophet"
	"github.com/fagongzi/util/format"
	"github.com/fagongzi/util/protoc"
)

func (s *store) ProphetBecomeLeader() {
	logger.Infof("*********Become prophet leader*********")
	s.bootOnce.Do(func() {
		s.doBootstrapCluster()
		close(s.pdStartedC)
	})
}

func (s *store) ProphetBecomeFollower() {
	logger.Infof("*********Become prophet follower*********")
	s.bootOnce.Do(func() {
		s.doBootstrapCluster()
		close(s.pdStartedC)
	})
}

func (s *store) doBootstrapCluster() {
	logger.Infof("create %d shards on bootstrap the cluster", s.opts.initShards)

	data, err := s.cfg.MetadataStorages[0].Get(storeIdentKey)
	if err != nil {
		logger.Fatalf("load store meta failed with %+v", err)
	}

	if len(data) > 0 {
		id := format.MustBytesToUint64(data)
		if id > 0 {
			s.meta.meta.ID = id
			logger.Infof("load from local, store is %d", id)
			return
		}
	}

	id := s.mustAllocID()
	s.meta.meta.ID = id
	logger.Infof("init local store with id: %d", id)

	count := 0
	err = s.cfg.MetadataStorages[0].Scan(minKey, maxKey, func([]byte, []byte) (bool, error) {
		count++
		return false, nil
	}, false)
	if err != nil {
		logger.Fatalf("bootstrap store failed with %+v", err)
	}
	if count > 0 {
		logger.Fatalf("local store is not empty and has already hard data")
	}

	err = s.cfg.MetadataStorages[0].Set(storeIdentKey, format.Uint64ToBytes(id))
	if err != nil {
		logger.Fatal("save local store id failed with %+v", err)
	}

	var initShards []prophet.Resource
	if s.opts.initShards == 1 {
		s.keyConvertFunc = noConvert
		initShards = append(initShards, s.createInitShard(nil, nil))
	} else {
		s.keyConvertFunc = uint64Convert
		step := uint64(math.MaxUint64 / s.opts.initShards)
		start := uint64(0)
		end := start + step

		for i := uint64(0); i < s.opts.initShards; i++ {
			if s.opts.initShards-i == 1 {
				end = math.MaxUint64
			}

			initShards = append(initShards, s.createInitShard(format.Uint64ToBytes(start), format.Uint64ToBytes(end)))
			start = end
			end = start + step
		}
	}

	ok, err := s.pd.GetStore().PutBootstrapped(s.meta, initShards...)
	if err != nil {
		s.removeInitShardIfAlreadyBootstrapped(initShards...)
		logger.Warningf("bootstrap cluster failed with %+v", err)
	}
	if !ok {
		logger.Info("the cluster is already bootstrapped")
		s.removeInitShardIfAlreadyBootstrapped(initShards...)
	}

	s.pd.GetRPC().TiggerContainerHeartbeat()
}

func (s *store) createInitShard(start, end []byte) prophet.Resource {
	shardID := s.mustAllocID()
	logger.Infof("alloc shard %d", shardID)

	peerID := s.mustAllocID()
	logger.Infof("alloc peer %d for shard %d",
		peerID,
		shardID)

	shard := metapb.Shard{}
	shard.ID = shardID
	shard.Epoch.ShardVer = 1
	shard.Epoch.ConfVer = 1
	shard.Start = start
	shard.End = end
	shard.Peers = append(shard.Peers, metapb.Peer{
		ID:      peerID,
		StoreID: s.meta.meta.ID,
	})

	wb := s.cfg.MetadataStorages[0].NewWriteBatch()

	// shard local state
	wb.Set(getStateKey(shard.ID), protoc.MustMarshal(&raftpb.ShardLocalState{Shard: shard}))

	// shard raft state
	raftState := new(raftpb.RaftLocalState)
	raftState.LastIndex = raftInitLogIndex
	raftState.HardState = protoc.MustMarshal(&etcdraftpb.HardState{
		Term:   raftInitLogTerm,
		Commit: raftInitLogIndex,
	})
	wb.Set(getRaftStateKey(shard.ID), protoc.MustMarshal(raftState))

	// shard raft apply state
	applyState := new(raftpb.RaftApplyState)
	applyState.AppliedIndex = raftInitLogIndex
	applyState.TruncatedState = raftpb.RaftTruncatedState{
		Term:  raftInitLogTerm,
		Index: raftInitLogIndex,
	}
	wb.Set(getApplyStateKey(shard.ID), protoc.MustMarshal(applyState))

	err := s.cfg.MetadataStorages[0].Write(wb, true)
	if err != nil {
		logger.Fatalf("create init shard failed, errors:\n %+v", err)
	}

	logger.Infof("create init shard %d succeed", shard.ID)
	return newResourceAdapter(shard)
}

func (s *store) removeInitShardIfAlreadyBootstrapped(initShards ...prophet.Resource) {
	wb := s.cfg.MetadataStorages[0].NewWriteBatch()

	for _, res := range initShards {
		id := res.ID()
		wb.Delete(getStateKey(id))
		wb.Delete(getRaftStateKey(id))
		wb.Delete(getApplyStateKey(id))
	}

	err := s.cfg.MetadataStorages[0].Write(wb, true)
	if err != nil {
		logger.Fatalf("remove init shards failed with %d", err)
	}

	logger.Info("all init shards is already removed from store")
}

func (s *store) mustAllocID() uint64 {
	id, err := s.pd.GetRPC().AllocID()
	if err != nil {
		logger.Fatalf("alloc id failed with %+v", err)
	}

	return id
}

type containerAdapter struct {
	meta metapb.Store
}

func newContainer(meta metapb.Store) prophet.Container {
	return &containerAdapter{
		meta: meta,
	}
}

func (ca *containerAdapter) ShardAddr() string {
	return ca.meta.ShardAddr
}

func (ca *containerAdapter) SetID(id uint64) {
	ca.meta.ID = id
}

func (ca *containerAdapter) ID() uint64 {
	return ca.meta.ID
}

func (ca *containerAdapter) Labels() []prophet.Pair {
	var values []prophet.Pair
	for _, label := range ca.meta.Labels {
		values = append(values, prophet.Pair{Key: label.Key, Value: label.Value})
	}
	return values
}

func (ca *containerAdapter) State() prophet.State {
	switch ca.meta.State {
	case metapb.StoreUP:
		return prophet.UP
	case metapb.StoreDown:
		return prophet.Down
	case metapb.StoreTombstone:
		return prophet.Tombstone
	default:
		return prophet.Down
	}
}

func (ca *containerAdapter) ActionOnJoinCluster() prophet.Action {
	return prophet.NoneAction
}

func (ca *containerAdapter) Clone() prophet.Container {
	value := &containerAdapter{}
	data, _ := ca.Marshal()
	value.Unmarshal(data)
	return value
}

func (ca *containerAdapter) Marshal() ([]byte, error) {
	return protoc.MustMarshal(&ca.meta), nil
}

func (ca *containerAdapter) Unmarshal(data []byte) error {
	protoc.MustUnmarshal(&ca.meta, data)
	return nil
}

type resourceAdapter struct {
	meta metapb.Shard
}

func newResourceAdapter(meta metapb.Shard) prophet.Resource {
	return &resourceAdapter{
		meta: meta,
	}
}

func (ra *resourceAdapter) SetID(id uint64) {
	ra.meta.ID = id
}

func (ra *resourceAdapter) ID() uint64 {
	return ra.meta.ID
}

func (ra *resourceAdapter) Peers() []*prophet.Peer {
	var peers []*prophet.Peer
	for _, peer := range ra.meta.Peers {
		peers = append(peers, &prophet.Peer{
			ID:          peer.ID,
			ContainerID: peer.StoreID,
		})
	}
	return peers
}

func (ra *resourceAdapter) Labels() []prophet.Pair {
	return nil
}

func (ra *resourceAdapter) SetPeers(peers []*prophet.Peer) {
	var values []metapb.Peer
	for _, peer := range peers {
		values = append(values, metapb.Peer{ID: peer.ID, StoreID: peer.ContainerID})
	}
	ra.meta.Peers = values
}

func (ra *resourceAdapter) ScaleCompleted(id uint64) bool {
	return true
}

func (ra *resourceAdapter) Stale(other prophet.Resource) bool {
	otherT := other.(*resourceAdapter)
	return isEpochStale(otherT.meta.Epoch, ra.meta.Epoch)
}

func (ra *resourceAdapter) Changed(other prophet.Resource) bool {
	otherT := other.(*resourceAdapter)
	return isEpochStale(ra.meta.Epoch, otherT.meta.Epoch)
}

func (ra *resourceAdapter) Clone() prophet.Resource {
	value := &resourceAdapter{}
	data, _ := ra.Marshal()
	value.Unmarshal(data)
	return value
}

func (ra *resourceAdapter) Marshal() ([]byte, error) {
	return protoc.MustMarshal(&ra.meta), nil
}

func (ra *resourceAdapter) Unmarshal(data []byte) error {
	protoc.MustUnmarshal(&ra.meta, data)
	return nil
}

type prophetAdapter struct {
	s *store
}

func newProphetAdapter(s *store) prophet.Adapter {
	return &prophetAdapter{
		s: s,
	}
}

func (pa *prophetAdapter) NewResource() prophet.Resource {
	return &resourceAdapter{}
}

func (pa *prophetAdapter) NewContainer() prophet.Container {
	return &containerAdapter{}
}

func (pa *prophetAdapter) FetchLeaderResources() []uint64 {
	var values []uint64
	leaders := 0
	shards := 0
	pa.s.foreachPR(func(pr *peerReplica) bool {
		pr.checkPeers()
		if pr.isLeader() {
			values = append(values, pr.shardID)
			leaders++
		}

		shards++
		return true
	})

	metric.SetShardsOnStore(shards, leaders)
	return values
}

func (pa *prophetAdapter) FetchResourceHB(id uint64) *prophet.ResourceHeartbeatReq {
	pr := pa.s.getPR(id, true)
	if pr == nil {
		return nil
	}

	req := new(prophet.ResourceHeartbeatReq)
	req.Resource = newResourceAdapter(pr.ps.shard)
	req.LeaderPeer = &prophet.Peer{ID: pr.peer.ID, ContainerID: pr.peer.StoreID}
	req.PendingPeers = pr.collectPendingPeers()
	req.DownPeers = pr.collectDownPeers(pa.s.opts.maxPeerDownTime)
	req.ContainerID = pa.s.meta.meta.ID

	return req
}

func (pa *prophetAdapter) FetchContainerHB() *prophet.ContainerHeartbeatReq {
	// prophet bootstrap not complete
	if pa.s.meta.meta.ID == 0 {
		return nil
	}

	req := new(prophet.ContainerHeartbeatReq)
	req.Container = pa.s.meta.Clone()
	req.Busy = false
	req.SendingSnapCount = pa.s.sendingSnapCount
	req.ReceivingSnapCount = pa.s.reveivingSnapCount

	pa.s.foreachPR(func(pr *peerReplica) bool {
		if pr.isLeader() {
			req.LeaderCount++
		}

		if pr.ps.isApplyingSnapshot() {
			req.ApplyingSnapCount++
		}

		req.ReplicaCount++
		return true
	})

	if pa.s.opts.useMemoryAsStorage {
		stats, err := util.MemStats()
		if err != nil {
			logger.Errorf("fetch store storage based on memory failed with %+v",
				err)
			return nil
		}

		req.StorageCapacity = stats.Total
		req.StorageAvailable = stats.Available
	} else {
		stats, err := util.DiskStats(pa.s.opts.dataPath)
		if err != nil {
			logger.Errorf("fetch store storage based on %s failed with %+v",
				pa.s.opts.dataPath,
				err)
			return nil
		}

		req.StorageCapacity = stats.Total
		req.StorageAvailable = stats.Free
	}

	metric.SetStorageOnStore(req.StorageCapacity, req.StorageAvailable)
	return req
}

func (pa *prophetAdapter) ResourceHBInterval() time.Duration {
	return pa.s.opts.shardHeartbeatDuration
}

func (pa *prophetAdapter) ContainerHBInterval() time.Duration {
	return pa.s.opts.storeHeartbeatDuration

}

func (pa *prophetAdapter) HBHandler() prophet.HeartbeatHandler {
	return pa
}

func (pa *prophetAdapter) ChangeLeader(resourceID uint64, newLeader *prophet.Peer) {
	pr := pa.s.getPR(resourceID, true)
	if pr == nil {
		return
	}

	logger.Infof("shard %d schedule change leader to peer %+v ",
		resourceID,
		newLeader)

	pr.onAdmin(&raftcmdpb.AdminRequest{
		CmdType: raftcmdpb.TransferLeader,
		Transfer: &raftcmdpb.TransferLeaderRequest{
			Peer: metapb.Peer{
				ID:      newLeader.ID,
				StoreID: newLeader.ContainerID,
			},
		},
	})
}

func (pa *prophetAdapter) ChangePeer(resourceID uint64, peer *prophet.Peer, changeType prophet.ChangePeerType) {
	pr := pa.s.getPR(resourceID, true)
	if pr == nil {
		return
	}

	p := metapb.Peer{
		ID:      peer.ID,
		StoreID: peer.ContainerID,
	}

	var ct raftcmdpb.ChangePeerType
	if changeType == prophet.AddPeer {
		ct = raftcmdpb.AddNode
	} else if changeType == prophet.RemovePeer {
		ct = raftcmdpb.RemoveNode
	}

	pr.onAdmin(&raftcmdpb.AdminRequest{
		CmdType: raftcmdpb.ChangePeer,
		ChangePeer: &raftcmdpb.ChangePeerRequest{
			ChangeType: ct,
			Peer:       p,
		},
	})
}

func (pa *prophetAdapter) ScaleResource(resourceID uint64, byContainerID uint64) {

}
