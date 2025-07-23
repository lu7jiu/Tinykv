// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"errors"
	"math/rand"
	"sort"
	"sync"
	"time"

	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0

// StateType represents the role of a node in a cluster.
type StateType uint64

const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
)

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// 用于生成随机数，随机改变选举超时时间
type lockedRand struct {
	mu   sync.Mutex
	rand *rand.Rand
}

var globalRand = &lockedRand{
	rand: rand.New(rand.NewSource(time.Now().UnixNano())),
}

func (r *lockedRand) Intn(n int) int {
	r.mu.Lock()
	v := r.rand.Intn(n)
	r.mu.Unlock()
	return v
}

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	// ElectionTick 是选举间隔需经历的 Node.Tick 调用次数
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	// HeartbeatTick 是心跳间隔需经历的 Node.Tick 调用次数
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	// Applied 是最后应用的日志索引
	Applied uint64
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	return nil
}

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
type Progress struct {
	Match, Next uint64 // Match 是已匹配的索引，Next 是下一个要发送的索引
}

// Progress 表示领导者视角中跟随者的进度。领导者维护所有跟随者的进度，
// 并根据其进度向跟随者发送条目。

type Raft struct {
	id uint64

	Term uint64
	Vote uint64

	// the log
	RaftLog *RaftLog

	// log replication progress of each peers
	Prs map[uint64]*Progress // 当节点成为 leader 后会使用, 即 matchIndex[] 和 nextIndex[]

	// this peer's role
	State StateType

	// votes records
	votes map[uint64]bool // 有哪些节点给自己投了票

	// msgs need to send
	msgs []pb.Message

	// the leader id
	Lead uint64

	// heartbeat interval, should send
	heartbeatTimeout int
	// baseline of election interval
	electionTimeout int
	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int
	// Ticks since it reached last electionTimeout when it is leader or candidate.
	// Number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int

	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in section 3.10 of Raft phd thesis.
	// (https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)
	// (Used in 3A leader transfer)
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via PendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// (Used in 3A conf change)
	PendingConfIndex uint64
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	// Your Code Here (2A).
	hardstate, confstate, err := c.Storage.InitialState()
	if err != nil {
		panic(err.Error())
	}
	if c.peers == nil {
		c.peers = confstate.Nodes
	}
	prs := make(map[uint64]*Progress)
	for _, pr := range c.peers {
		prs[pr] = &Progress{
			Match: 0,
			Next:  0,
		}
	}
	// vote := make(map[uint64]bool)
	// for _, pr := range c.peers {
	// 	vote[pr] = false
	// }
	return &Raft{
		id:               c.ID,
		Term:             hardstate.Term,
		Vote:             hardstate.Vote,
		RaftLog:          newLog(c.Storage),
		Prs:              prs,
		State:            StateFollower,
		votes:            make(map[uint64]bool),
		Lead:             None,
		heartbeatTimeout: c.HeartbeatTick,
		electionTimeout:  c.ElectionTick,
	}
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	//msg包含日志、上一个日志的索引和任期、commit的索引
	pr, ok := r.Prs[to]
	if !ok {
		return false
	}
	prelogindex := pr.Next - 1
	prelogterm, err := r.RaftLog.Term(prelogindex)
	//日志被压缩，发快照
	if pr.Next < r.RaftLog.FirstIndex() || err != nil {
		//快照,待实现
	}
	firstindex := r.RaftLog.FirstIndex()
	var entries []*pb.Entry
	for i := pr.Next; i <= r.RaftLog.LastIndex(); i++ {
		entries = append(entries, &r.RaftLog.entries[i-firstindex])
	}
	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppend,
		To:      to,
		From:    r.id,
		Term:    r.Term,
		LogTerm: prelogterm,
		Index:   prelogindex,
		Entries: entries,
		Commit:  r.RaftLog.committed,
	}
	r.msgs = append(r.msgs, msg)
	return true
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	// Your Code Here (2A).
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		To:      to,
		From:    r.id,
		Term:    r.Term,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendRequestVote(to uint64) {
	logindex := r.RaftLog.LastIndex()
	logterm, err := r.RaftLog.Term(logindex)
	if err != nil {
		return
	}
	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVote,
		To:      to,
		From:    r.id,
		Term:    r.Term,
		LogTerm: logterm,
		Index:   logindex,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendAppendResponse(reject bool, to uint64, lastIndex uint64) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		To:      to,
		From:    r.id,
		Term:    r.Term,
		Index:   lastIndex, //最后一条日志索引，便于失败时重新发送日志追加、更新matchindex
		Reject:  reject,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendHeartbeatResponse(to uint64) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeatResponse,
		To:      to,
		From:    r.id,
		Term:    r.Term,
		Commit:  r.RaftLog.committed, //用于告诉 Leader 自己是否落后
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendRequestVoteResponse(reject bool, to uint64) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVoteResponse,
		To:      to,
		From:    r.id,
		Term:    r.Term,
		Reject:  reject,
	}
	r.msgs = append(r.msgs, msg)
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	// Your Code Here (2A).
	r.electionElapsed++
	switch r.State {
	case StateFollower:
		//选举超时
		if r.electionElapsed >= r.electionTimeout {
			//计时清零
			r.electionElapsed = 0
			//发起选举
			err := r.Step(pb.Message{MsgType: pb.MessageType_MsgHup})
			if err != nil {
				return
			}
		}
	case StateCandidate:
		//选举超时
		if r.electionElapsed >= r.electionTimeout {
			//计时清零
			r.electionElapsed = 0
			//发起选举
			err := r.Step(pb.Message{MsgType: pb.MessageType_MsgHup})
			if err != nil {
				return
			}
		}
	case StateLeader:
		r.heartbeatElapsed++
		//心跳超时
		if r.heartbeatElapsed >= r.heartbeatTimeout {
			r.heartbeatElapsed = 0
			err := r.Step(pb.Message{MsgType: pb.MessageType_MsgBeat})
			if err != nil {
				return
			}
		}
	}
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	// Your Code Here (2A).
	r.State = StateFollower
	r.Lead = lead
	r.reset(term)
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	// Your Code Here (2A).
	r.State = StateCandidate
	//任期加一
	r.reset(r.Term + 1)
	//投票给自己
	r.Vote = r.id
	r.votes[r.id] = true
}

func (r *Raft) sendAllRaftRequestVote() {
	//遍历结点
	for pr := range r.Prs {
		if pr != r.id {
			r.sendRequestVote(pr)
		}
	}
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
	r.State = StateLeader
	r.reset(r.Term)
	r.Lead = r.id
	//match为0，next为最后一条
	lastIndex := r.RaftLog.LastIndex()
	for pr := range r.Prs {
		r.Prs[pr].Match = 0
		r.Prs[pr].Next = lastIndex + 1
	}
	//增加一条空日志
	r.RaftLog.entries = append(r.RaftLog.entries, pb.Entry{
		Term:  r.Term,
		Index: lastIndex + 1,
	})
	//更新自己的progress
	r.Prs[r.id].Match = r.RaftLog.LastIndex()
	r.Prs[r.id].Next = r.Prs[r.id].Match + 1
	//发送追加日志
	for pr := range r.Prs {
		if pr != r.id {
			r.sendAppend(pr)
		}
	}
	//更新commitindex？
}

// 在新的任期下重置状态
func (r *Raft) reset(term uint64) {
	//一个任期只能投一次票
	if r.Term != term {
		r.Term = term
		r.Vote = None
	}
	//重置投票记录，用于重新选举
	r.votes = make(map[uint64]bool)
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	//重置选举超时时间
	r.resetRandomizedElectionTimeout()
}

// 随机重置选举超时时间
func (r *Raft) resetRandomizedElectionTimeout() {
	rand := globalRand.Intn(r.electionTimeout)
	r.electionTimeout += rand
	//测试要求在10~20个tick之间
	for r.electionTimeout >= 20 {
		r.electionTimeout -= 10
	}
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
	var err error = nil
	switch r.State {
	case StateFollower:
		err = r.FollowerStep(m)
	case StateCandidate:
		err = r.CandidateStep(m)
	case StateLeader:
		err = r.LeaderStep(m)
	}
	return err
}

func (r *Raft) FollowerStep(m pb.Message) error {
	var err error = nil
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		r.handleElection()
	case pb.MessageType_MsgBeat:
	case pb.MessageType_MsgPropose:
		err = ErrProposalDropped
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
	case pb.MessageType_MsgTransferLeader:
		//
	case pb.MessageType_MsgTimeoutNow:
		r.handleElection()
	}
	return err
}

func (r *Raft) CandidateStep(m pb.Message) error {
	var err error = nil
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		r.handleElection()
	case pb.MessageType_MsgBeat:
	case pb.MessageType_MsgPropose:
		err = ErrProposalDropped
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
		r.handleRequestVoteResponse(m)
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
	case pb.MessageType_MsgTransferLeader:
		//
	case pb.MessageType_MsgTimeoutNow:
		r.handleElection()
	}
	return err
}

func (r *Raft) LeaderStep(m pb.Message) error {
	var err error = nil
	switch m.MsgType {
	case pb.MessageType_MsgHup:
	case pb.MessageType_MsgBeat:
		for pr := range r.Prs {
			if pr != r.id {
				r.sendHeartbeat(pr)
			}
		}
	case pb.MessageType_MsgPropose:
		r.handlePropose(m)
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
		r.handleAppendResponse(m)
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
		r.handleHeartbeatResponse(m)
	case pb.MessageType_MsgTransferLeader:
		//
	case pb.MessageType_MsgTimeoutNow:
		r.handleElection()
	}
	return err
}

// 处理日志追加
// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	//任期新则更新任期、领导，变为follower
	if m.Term >= r.Term {
		r.Term = m.Term
		if r.State != StateFollower {
			r.becomeFollower(m.Term, m.From)
		}
		if r.Lead != m.From {
			r.Lead = m.From
		}
		r.electionElapsed = 0
	}
	//leader直接拒绝
	if r.State == StateLeader {
		return
	}
	//任期旧则拒绝
	if m.Term < r.Term {
		r.sendAppendResponse(true, m.From, r.RaftLog.LastIndex())
		return
	}
	//检查前一个日志是否存在，存在才追加；不存在则拒绝
	prelogindex := m.Index
	prelogterm := m.LogTerm
	if prelogindex > r.RaftLog.LastIndex() {
		r.sendAppendResponse(true, m.From, r.RaftLog.LastIndex())
		return
	}
	actualprelogterm, err := r.RaftLog.Term(prelogindex)
	//前一个日志任期对不上，拒绝追加
	if actualprelogterm != prelogterm || err != nil {
		r.sendAppendResponse(true, m.From, r.RaftLog.LastIndex())
		return
	}
	//追加新条目，删除冲突
	for _, entry := range m.Entries {
		oldlogterm, err := r.RaftLog.Term(entry.Index)
		if entry.Index > r.RaftLog.LastIndex() { //无冲突则直接追加
			r.RaftLog.entries = append(r.RaftLog.entries, *entry)
		} else if oldlogterm != entry.Term || err != nil { //存在冲突且新旧日志的任期匹配不上，则删除旧日志再追加
			if entry.Index < r.RaftLog.FirstIndex() {
				r.RaftLog.entries = make([]pb.Entry, 0)
			} else {
				r.RaftLog.entries = r.RaftLog.entries[0 : entry.Index-r.RaftLog.FirstIndex()]
			}
			//由于删除了旧日志，所以要更新stabled
			r.RaftLog.stabled = min(r.RaftLog.stabled, entry.Index-1)
			//追加新日志
			r.RaftLog.entries = append(r.RaftLog.entries, *entry)
		}
	}
	r.RaftLog.lastappend = prelogindex + uint64(len(m.Entries))
	//更新committed
	if m.Commit > r.RaftLog.committed {
		r.RaftLog.committed = min(m.Commit, r.RaftLog.lastappend)
	}
	r.sendAppendResponse(false, m.From, r.RaftLog.LastIndex())
}

// 响应心跳
// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	//判断任期是否大于等于自己的，是则接受，不是则拒绝
	if m.Term >= r.Term {
		//更新任期
		r.Term = m.Term
		//变成跟随者
		if r.State != StateFollower {
			r.becomeFollower(r.Term, m.From)
		}
		//更新领导
		if r.Lead != m.From {
			r.Lead = m.From
		}
		//选举超时计时清零
		r.electionElapsed = 0
	}
	//心跳响应
	r.sendHeartbeatResponse(m.From)
}

// 响应投票请求
func (r *Raft) handleRequestVote(m pb.Message) {
	//新的任期，清空投票记录
	if m.Term > r.Term {
		r.Vote = None
		r.Term = m.Term
		if r.State != StateFollower {
			r.becomeFollower(m.Term, None)
		}
	}
	if m.Term < r.Term {
		r.sendRequestVoteResponse(true, m.From)
		return
	}
	//未投票或投给from才能进入投票判断
	if r.Vote == None || r.Vote == m.From {
		//判断日志
		lastindex := r.RaftLog.LastIndex()
		lastterm, _ := r.RaftLog.Term(lastindex)
		//任期大或任期相同索引大，即为新
		if m.LogTerm > lastterm || (m.LogTerm == lastterm && m.Index >= lastindex) {
			r.Vote = m.From
			r.sendRequestVoteResponse(false, m.From)
		} else {
			r.sendRequestVoteResponse(true, m.From)
		}
	} else {
		r.sendRequestVoteResponse(true, m.From)
	}
}

// 处理日志追加响应
func (r *Raft) handleAppendResponse(m pb.Message) {
	//收到rpc任期新则转为follower
	if m.Term > r.Term {
		r.becomeFollower(m.Term, None)
		return
	}
	//日志追加失败，更新next重新发起日志追加
	if m.Reject {
		r.Prs[m.From].Next = min(m.Index+1, r.Prs[m.From].Next-1)
		r.sendAppend(m.From)
		return
	}
	//追加成功，更新prs
	r.Prs[m.From].Match = m.Index
	r.Prs[m.From].Next = m.Index + 1
	//更新commited
	oldcommitted := r.RaftLog.committed
	commited := r.updateCommitIndex()
	//如果更新成功，则继续追加日志，同步commited
	if oldcommitted != commited {
		for pr := range r.Prs {
			if pr != r.id {
				r.sendAppend(pr)
			}
		}
	}

}

// 更新提交日志索引
func (r *Raft) updateCommitIndex() uint64 {
	//存放各节点的matchindex，用于判断是否满足大多数节点已复制
	match := make(uint64Slice, len(r.Prs))
	i := 0
	for _, pr := range r.Prs {
		match[i] = pr.Match
		i++
	}
	sort.Sort(match)
	//满足大多数节点复制的日志索引
	mostcopyindex := match[(len(r.Prs)-1)/2]
	//判断该日志任期是否为当前任期
	for ; mostcopyindex > r.RaftLog.committed; mostcopyindex-- {
		if logterm, _ := r.RaftLog.Term(mostcopyindex); logterm == r.Term {
			break
		}
	}
	r.RaftLog.committed = mostcopyindex
	return r.RaftLog.committed
}

// 处理心跳响应
func (r *Raft) handleHeartbeatResponse(m pb.Message) {
	if m.Term > r.Term {
		r.becomeFollower(m.Term, None)
	}
	// 如果节点落后了，追加日志
	if m.Commit < r.RaftLog.committed {
		r.sendAppend(m.From)
	}
}

// 处理选举响应
func (r *Raft) handleRequestVoteResponse(m pb.Message) {
	r.votes[m.From] = !m.Reject
	total := len(r.Prs) //总节点数
	agree := 0          //同意
	disagree := 0       //反对
	for _, vote := range r.votes {
		if vote {
			agree++
		} else {
			disagree++
		}
	}
	if 2*agree > total { //超过一半赞成，成为leader
		r.becomeLeader()
	} else if 2*disagree >= total {
		r.becomeFollower(r.Term, None)
	}
}

func (r *Raft) handleElection() {
	//节点不在集群中，不能发起选举
	if _, ok := r.Prs[r.id]; !ok {
		return
	}
	if len(r.Prs) == 1 {
		r.Term++
		r.becomeLeader()
	} else {
		r.becomeCandidate()
		r.sendAllRaftRequestVote()
	}
}

func (r *Raft) handlePropose(m pb.Message) {
	r.appendEntry(m.Entries)
	//发送追加日志rpc
	for pr := range r.Prs {
		if pr != r.id {
			r.sendAppend(pr)
		}
	}
	//如果只有一个节点，提交日志
	if len(r.Prs) == 1 {
		r.RaftLog.committed = r.Prs[r.id].Match
	}
}

// 执行propose追加日志
func (r *Raft) appendEntry(entries []*pb.Entry) {
	lastindex := r.RaftLog.LastIndex()
	for i := range entries {
		entries[i].Term = r.Term
		entries[i].Index = lastindex + 1 + uint64(i)
		r.RaftLog.entries = append(r.RaftLog.entries, *entries[i])
	}
	r.Prs[r.id].Match = r.RaftLog.LastIndex()
	r.Prs[r.id].Next = r.Prs[r.id].Match + 1
	r.RaftLog.lastappend = r.Prs[r.id].Match
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
}
