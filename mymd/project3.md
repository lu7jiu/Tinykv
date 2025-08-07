# Project3 MultiRaftKV

## 3A
基于Project 2实现的Raft算法，添加成员变更和领导者变更功能。
### 成员变更
采用逐个添加或移除节点的方式实现。
配置变更始于调用领导者的`raft.RawNode.ProposeConfChange`，该方法会提议一个日志条目，其中`pb.Entry.EntryType`设置为`EntryConfChange`，`pb.Entry.Data`设置为输入的`pb.ConfChange`。当类型为`EntryConfChange`的日志条目被提交后，通过 `RawNode.ApplyConfChange`来应用它，根据`pb.ConfChange`的类型调用`raft.Raft.addNode`和`raft.Raft.removeNode`向该Raft节点添加或移除节点。
1.addNode：若新节点已存在则直接返回，否则在`Prs`中添加新节点的映射关系
2.removeNode：若节点不存在则返回，否则删除`Prs`中该节点的信息

- 若删除的节点是唯一的follower，且在追加日志后删除节点，则删除节点后leader可能无法收到追加日志响应，从而无法提交日志，因此在删除节点后需要判断是否有日志可以提交，并及时更新日志提交索引

### 领导者变更
引入`leadTransferee`：记录领导者转移目标的id；两种新的消息类型：`MsgTransferLeader`和`MsgTimeoutNow`
1.handleTransferLeader：处理领导者转移消息。

- 转移条件：接收者同步了最新的日志
- 若满足条件，则向接收者发送`MsgTimeoutNow`消息，接收者在收到 `MsgTimeoutNow`消息后，无论其选举超时时间如何，都应立即开始新的选举
- 若不满足条件，则向接收者发送`MsgAppend`消息，并停止接受新的提议，以避免陷入循环。成功追加日志后，领导者收到日志追加响应，重新尝试转移领导权。

### 问题及解决
1.`MsgTransferLeader`消息不是只会发送给leader，在`TestLeaderTransferToUpToDateNodeFromFollower3A`测试中，`MsgTransferLeader`的消息只发送给follower，因此需要添加follower和candidate处理`MsgTransferLeader`的方法：转发消息给leader
```go
// TestLeaderTransferToUpToDateNodeFromFollower verifies transferring should succeed
// if the transferee has the most up-to-date log entries when transfer starts.
// Not like TestLeaderTransferToUpToDateNode, where the leader transfer message
// is sent to the leader, in this test case every leader transfer message is sent
// to the follower.
func TestLeaderTransferToUpToDateNodeFromFollower3A(t *testing.T) {
	nt := newNetwork(nil, nil, nil)
	nt.send(pb.Message{From: 1, To: 1, MsgType: pb.MessageType_MsgHup})

	lead := nt.peers[1].(*Raft)

	if lead.Lead != 1 {
		t.Fatalf("after election leader is %d, want 1", lead.Lead)
	}

	// Transfer leadership to 2.
	nt.send(pb.Message{From: 2, To: 2, MsgType: pb.MessageType_MsgTransferLeader})

	checkLeaderTransferState(t, lead, StateFollower, 2)

	// After some log replication, transfer leadership back to 1.
	nt.send(pb.Message{From: 1, To: 1, MsgType: pb.MessageType_MsgPropose, Entries: []*pb.Entry{{}}})

	nt.send(pb.Message{From: 1, To: 1, MsgType: pb.MessageType_MsgTransferLeader})

	checkLeaderTransferState(t, lead, StateLeader, 1)
}
```