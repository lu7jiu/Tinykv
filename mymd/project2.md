# Project2 RaftKV

## 2A

实现基础的Raft算法，包括领导人选举、日志复制，并封装为`RawNode`接口供上层调用。

### Raft

1.tick()：将内部逻辑时钟推进一个 tick，从而驱动选举超时或心跳超时。保证领导者的地位，在领导者宕机时选出新的领导者。

- leader：心跳计时++，判断是否心跳超时，是则发送心跳。
- candidate、follower：选举计时++，判断是否选举超时，是则发起选举。

2.Step(m pb.Message)：节点收到信息时调用此方法来处理信息。不同的角色对不同的信息有不同的处理方式。

![Step实现思路](https://github.com/sakura-ysy/TinyKV-2022-doc/raw/main/doc/project2/%E5%9B%BE%E7%89%872.png "Step实现思路")

- 对于节点之间发送的信息，若信息发送者的任期小于接收者的，则拒绝，若任期大于接受者的，且接收者的角色不是follower，则转变为follower。
- 一个节点在一个任期中只能投一次票。
- 心跳响应信息中应该包含日志提交索引，领导者收到心跳响应时判断节点是否落后，落后则追加日志。
- 对于只有一个节点的情况：发起选举，直接成为领导人；领导人执行MsgPropose时，追加日志后直接提交日志。
- 测试要求选举超时时间在10~20个tick之间
- 节点成为领导者之后，需要追加一条空日志，注意更新日志提交索引（一般情况下在处理日志追加响应时更新日志提交索引，但如果只有一个节点，不会发送日志追加请求，则无法提交日志）。
  
### RawNode

1.HasReady()：判断 Raft 模块是否有需要上层处理的信息，包括状态变化、持久化日志、应用日志、发送信息等。
2.Ready()：获取上述提到的需要处理的内容。
3.Advance(rd Ready)：根据处理完的Ready内容来更新RawNode。

## 2B

整体流程：客户端发送请求-->`peerMsgHandler`将请求序列化作为data存入日志-->调用`RawNode.Propose()`追加日志，在raft集群中同步日志-->处理`ready`，将数据写入数据库，发送响应给客户端

```go
func (rw *raftWorker) run(closeCh <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	var msgs []message.Msg
	for {
		msgs = msgs[:0] // 清空缓冲区(保留底层数组)
		// 等待消息或关闭信号
		select {
		case <-closeCh:
			return // 收到关闭信号立即退出
		case msg := <-rw.raftCh:
			msgs = append(msgs, msg) // 收取第一条消息
		}
		// 批量收取剩余待处理消息
		pending := len(rw.raftCh)
		for i := 0; i < pending; i++ {
			msgs = append(msgs, <-rw.raftCh)
		}
		// 按region组织peer状态
		peerStateMap := make(map[uint64]*peerState)
		for _, msg := range msgs {
			peerState := rw.getPeerState(peerStateMap, msg.RegionID)
			if peerState == nil {
				continue // 找不到peer则跳过
			}
			// 创建消息处理器并处理消息
			newPeerMsgHandler(peerState.peer, rw.ctx).HandleMsg(msg)
		}
		// 处理每个peer的ready
		for _, peerState := range peerStateMap {
			newPeerMsgHandler(peerState.peer, rw.ctx).HandleRaftReady()
		}
	}
}
```
### PeerStorage
存储 Peer 相关数据，包含两个Badger实例：raftdb 和 kvdb
- raftdb：存储 Raft 日志和 RaftLocalState
- kvdb：存储键值数据、RegionLocalState、RaftApplyState。可以将 kvdb 视为 Raft 论文中提到的状态机
#### Append
实现持久化日志：使用WriteBatch，添加待持久化日志，删除冲突的日志条目
#### SaveReadyState
实现状态和日志持久化
1.判断硬状态是否有更新，将更新的硬状态保存在`ps.raftState.HardState`
2.判断有无待持久化日志：调用Append实现持久化，更新`ps.raftState.LastIndex`、`ps.raftState.LastTerm`
3.持久化RaftLocalState
4.批量写入raftdb
### peerMsgHandler
主要有两个函数：一个是 `HandleMsg`，另一个是 `HandleRaftReady`。
`HandleMsg`处理接收到的信息，消息处理完成后，Raft 节点会有一些状态更新，`HandleRaftReady`从Raft获取ready，并执行相应操作。
#### proposeRaftCommand
处理`RaftCmdRequest`，一个`RaftCmdRequest`包含多个`Request`，`Request`有四种类型：Get、Put、Delete、Snap。
对于每一个Request：
1.创建一个proposal（在应用日志后向客户端返回响应）
2.将请求信息序列化，作为日志的data，调用`d.RaftGroup.Propose`提议追加日志
#### HandleRaftReady
raft同步日志后，有ready需要处理
1.调用`d.peer.RaftGroup.HasReady()`判断是否有需要处理的ready
2.调用`d.peer.RaftGroup.Ready()`获取ready
3.调用`d.peerStorage.SaveReadyState`持久化状态、日志
4.调用`d.Send`发送消息
5.应用日志
  ①对于每条日志，根据请求的类型执行相应的操作，创建`RaftCmdResponse`，找到与日志匹配的proposal，使用`proposal.cb.Done`返回响应
  
  - 对于过时请求的proposal，返回响应让客户端知道并重试该命令

  ②更新RaftApplyState并写入kvdb
6.调用`d.RaftGroup.Advance`更新RawNode
### 问题及解决
1.网络分区：增加heartbeatResp记录leader收到的心跳响应，在leader选举超时时，若收到心跳响应少于半数，则表示发生网络分区，此时leader应发起选举，不能再做leader
2.一个`Request`对应一个`RaftCmdResponse`及`proposal`，而不是一个`RaftCmdRequest`对应一个`RaftCmdResponse`及`proposal`

## 2C
### 日志压缩
服务器会定期检查 Raft 日志的数量，并时不时丢弃超过阈值的日志条目。
根据`RaftLogGcCountLimit`定期检查是否需要进行 gc log。如果是，它将提出一个压缩日志请求（CompactLogRequest），包装在`RaftCmdRequest`的`AdminRequest`中。
```go
func (d *peerMsgHandler) onRaftGCLogTick() {
	d.ticker.schedule(PeerTickRaftLogGC)
	if !d.IsLeader() {
		return
	}

	appliedIdx := d.peerStorage.AppliedIndex()
	firstIdx, _ := d.peerStorage.FirstIndex()
	var compactIdx uint64
	if appliedIdx > firstIdx && appliedIdx-firstIdx >= d.ctx.cfg.RaftLogGcCountLimit {
		compactIdx = appliedIdx
	} else {
		return
	}

	y.Assert(compactIdx > 0)
	compactIdx -= 1
	if compactIdx < firstIdx {
		// In case compact_idx == first_idx before subtraction.
		return
	}

	term, err := d.RaftGroup.Raft.RaftLog.Term(compactIdx)
	if err != nil {
		log.Fatalf("appliedIdx: %d, firstIdx: %d, compactIdx: %d", appliedIdx, firstIdx, compactIdx)
		panic(err)
	}

	// Create a compact log request and notify directly.
	regionID := d.regionId
	request := newCompactLogRequest(regionID, d.Meta, compactIdx, term)
	d.proposeRaftCommand(request, nil)
}
```
我们需要完成处理日志压缩请求、应用日志压缩请求这两部分内容，即完善peer_msg_handler.go文件。
1.处理日志压缩请求：同2B部分。

- 添加一个proposal用于在应用后返回响应
- 将请求信息包装在日志中，调用`Propose`追加日志，在raft中同步日志。

2.应用日志：

- 更新`applystate.TruncatedState`并写入数据库
- 调用`d.ScheduleCompactLog(compactindex)`，通过`ScheduleCompactLog`向`raftlog-gc`工作线程调度一个任务。`Raftlog-gc`工作线程会异步执行实际的日志删除工作。

### 快照
1.发送快照：leader向follower发送日志追加请求时，若follower落后太多，由于日志压缩，待追加的日志可能已经被压缩，此时需要发送快照。

- 调用`RaftLog.storage.Snapshot()`获取快照，将快照包装在`MsgSnapshot`类型的信息中发送给follower。
  
2.处理快照：follower调用`handleSnapshot`来处理该消息，即从消息中的`SnapshotMetadata`恢复Raft的内部状态，如任期、提交索引和成员信息等。

- 消息发送者任期小直接拒绝
- 更新任期、角色、领导，重置选举超时计时
- 若快照中包含的最后一条日志索引小于等于当前已提交的日志，则说明快照消息已过时，直接返回即可
- 将消息中的快照存放在`RaftLog.pendingSnapshot`中
- 更新`stabled、committed、applied`为快照的最后一条日志的索引
- 若有集群成员变更，则更新`Prs`
- 若快照之后没有日志，则添加一条空日志，用于标记最新日志的索引和任期，便于后续追加日志
  
3.应用快照：若raft节点的`pendingSnapshot`非空，表示有待处理的快照，通过`Ready`来反馈，在处理`Ready`时应用快照。

- 调用`clearMeta`、`clearExtraData`删除数据库中的日志、状态、过时的数据
- 更新`applyState`、`raftState`、`snapState`，并写入数据库
- 通过`ps.regionSched`发送`RegionTaskApply`任务给`region worker`处理
- 返回`ApplySnapResult`，存放应用快照前后的Region信息
- 若Region有变化，则更新`peerMsgHandler.ctx.storeMeta`的内容
- 处理完Ready后调用`Advance`更新节点时，需清空`pendingSnapshot`，调用`maybeCompact`清空RaftLog中被压缩的日志

### 问题及解决
1.发送`RegionTaskApply`任务时应该发送指针类型的数据给`ps.regionSched`，若发送值类型，接收方完成任务后，向`Notifier`发送信号时，发送方无法感知。
```go
type PeerStorage struct {
	// current region information of the peer
	region *metapb.Region
	// current raft state of the peer
	raftState *rspb.RaftLocalState
	// current apply state of the peer
	//状态机
	applyState *rspb.RaftApplyState

	// current snapshot state
	snapState snap.SnapState
	// regionSched used to schedule task to region worker
	regionSched chan<- worker.Task
	// generate snapshot tried count
	snapTriedCnt int
	// Engine include two badger instance: Raft and Kv
	Engines *engine_util.Engines
	// Tag used for logging
	Tag string
}

type Task interface{}
```
Task是空接口，发送哪种类型的数据都不会报错。参考`Snapshot`中使用`ps.regionSched`部分：
```go
func (ps *PeerStorage) Snapshot() (eraftpb.Snapshot, error) {
	var snapshot eraftpb.Snapshot
	if ps.snapState.StateType == snap.SnapState_Generating {
		select {
		case s := <-ps.snapState.Receiver:
			if s != nil {
				snapshot = *s
			}
		default:
			return snapshot, raft.ErrSnapshotTemporarilyUnavailable
		}
		ps.snapState.StateType = snap.SnapState_Relax
		if snapshot.GetMetadata() != nil {
			ps.snapTriedCnt = 0
			if ps.validateSnap(&snapshot) {
				return snapshot, nil
			}
		} else {
			log.Warnf("%s failed to try generating snapshot, times: %d", ps.Tag, ps.snapTriedCnt)
		}
	}

	if ps.snapTriedCnt >= 5 {
		err := errors.Errorf("failed to get snapshot after %d times", ps.snapTriedCnt)
		ps.snapTriedCnt = 0
		return snapshot, err
	}

	log.Infof("%s requesting snapshot", ps.Tag)
	ps.snapTriedCnt++
	ch := make(chan *eraftpb.Snapshot, 1)
	ps.snapState = snap.SnapState{
		StateType: snap.SnapState_Generating,
		Receiver:  ch,
	}
	// schedule snapshot generate task
	ps.regionSched <- &runner.RegionTaskGen{
		RegionId: ps.region.GetId(),
		Notifier: ch,
	}
	return snapshot, raft.ErrSnapshotTemporarilyUnavailable
}
```