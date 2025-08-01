package raftstore

import (
	"sync"

	"github.com/pingcap-incubator/tinykv/kv/raftstore/message"
)

// raftWorker is responsible for run raft commands and apply raft logs.
type raftWorker struct {
	pr *router

	// receiver of messages should sent to raft, including:
	// * raft command from `raftStorage`
	// * raft inner messages from other peers sent by network
	raftCh chan message.Msg //worker接收消息的主要通道
	ctx    *GlobalContext

	closeCh <-chan struct{}
}

func newRaftWorker(ctx *GlobalContext, pm *router) *raftWorker {
	return &raftWorker{
		raftCh: pm.peerSender,
		ctx:    ctx,
		pr:     pm,
	}
}

// run runs raft commands.
// On each loop, raft commands are batched by channel buffer.
// After commands are handled, we collect apply messages by peers, make a applyBatch, send it to apply channel.
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
		// 处理每个peer的Raft就绪状态
		for _, peerState := range peerStateMap {
			newPeerMsgHandler(peerState.peer, rw.ctx).HandleRaftReady()
		}
	}
}

func (rw *raftWorker) getPeerState(peersMap map[uint64]*peerState, regionID uint64) *peerState {
	// 首先检查缓存
	peer, ok := peersMap[regionID]
	if !ok {
		// 缓存未命中则从router查找
		peer = rw.pr.get(regionID)
		if peer == nil {
			return nil // 找不到peer返回nil
		}
		// 将找到的peer加入缓存
		peersMap[regionID] = peer
	}
	return peer
}
