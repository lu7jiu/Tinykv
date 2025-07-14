package server

import (
	"context"

	"github.com/pingcap-incubator/tinykv/kv/coprocessor"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/storage/raft_storage"
	"github.com/pingcap-incubator/tinykv/kv/transaction/latches"
	"github.com/pingcap-incubator/tinykv/kv/transaction/mvcc"
	coppb "github.com/pingcap-incubator/tinykv/proto/pkg/coprocessor"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/tinykvpb"
	"github.com/pingcap/tidb/kv"
)

var _ tinykvpb.TinyKvServer = new(Server)

// Server is a TinyKV server, it 'faces outwards', sending and receiving messages from clients such as TinySQL.
type Server struct {
	storage storage.Storage

	// (Used in 4B)
	Latches *latches.Latches

	// coprocessor API handler, out of course scope
	copHandler *coprocessor.CopHandler
}

func NewServer(storage storage.Storage) *Server {
	return &Server{
		storage: storage,
		Latches: latches.NewLatches(),
	}
}

// The below functions are Server's gRPC API (implements TinyKvServer).

// Raft commands (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Raft(stream tinykvpb.TinyKv_RaftServer) error {
	return server.storage.(*raft_storage.RaftStorage).Raft(stream)
}

// Snapshot stream (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Snapshot(stream tinykvpb.TinyKv_SnapshotServer) error {
	return server.storage.(*raft_storage.RaftStorage).Snapshot(stream)
}

// Transactional API.
func (server *Server) KvGet(_ context.Context, req *kvrpcpb.GetRequest) (*kvrpcpb.GetResponse, error) {
	// Your Code Here (4B).
	//判断key上是否存在ts早于startts的锁，不存在才可以读取value
	response := &kvrpcpb.GetResponse{}
	//创建reader
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionerror, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionerror.RequestErr
			return response, nil
		}
		return nil, err
	}
	defer reader.Close()
	//创建事务
	txn := mvcc.NewMvccTxn(reader, req.Version)
	//获取key上的锁
	lock, err := txn.GetLock(req.Key)
	if err != nil {
		if regionerror, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionerror.RequestErr
			return response, nil
		}
		return nil, err
	}
	if lock != nil && lock.Ts <= req.Version {
		response.Error = &kvrpcpb.KeyError{
			Locked: &kvrpcpb.LockInfo{
				PrimaryLock: lock.Primary,
				LockVersion: lock.Ts,
				Key:         req.Key,
				LockTtl:     lock.Ttl,
			},
		}
		return response, nil
	}
	val, err := txn.GetValue(req.Key)
	if err != nil {
		if regionerror, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionerror.RequestErr
			return response, nil
		}
		return nil, err
	}
	//判断val是否存在
	if val == nil {
		response.NotFound = true
	}
	response.Value = val
	return response, nil
}

func (server *Server) KvPrewrite(_ context.Context, req *kvrpcpb.PrewriteRequest) (*kvrpcpb.PrewriteResponse, error) {
	// Your Code Here (4B).
	//检查要写入的 key 是否存在大于 startTs 的 Write，如果存在，直接放弃，说明在事务开启后，已经存在写入并已提交，也就是存在写-写冲突
	//检查要写入 key 的数据是否存在 Lock（任意 startTs下的 Lock），即检测是否存在写写冲突，如果存在直接放弃
	response := &kvrpcpb.PrewriteResponse{}
	//创建reader
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionerror, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionerror.RequestErr
			return response, nil
		}
		return nil, err
	}
	defer reader.Close()
	//创建事务
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	var keyerrors []*kvrpcpb.KeyError //存放冲突
	//遍历检查是否存在写冲突，不存在则写入Lock和Default
	for _, Mutation := range req.Mutations {
		write, committs, err := txn.MostRecentWrite(Mutation.Key)
		if err != nil {
			if regionerror, ok := err.(*raft_storage.RegionError); ok {
				response.RegionError = regionerror.RequestErr
				return response, nil
			}
			return nil, err
		}
		//存在 committs 大于 startTs 的 Write，写冲突
		if write != nil && committs >= req.StartVersion {
			keyerrors = append(keyerrors, &kvrpcpb.KeyError{
				Conflict: &kvrpcpb.WriteConflict{
					StartTs:    req.StartVersion,
					ConflictTs: committs,
					Key:        Mutation.Key,
					Primary:    req.PrimaryLock,
				},
			})
			continue
		}
		//检查key上是否存在lock
		lock, err := txn.GetLock(Mutation.Key)
		if err != nil {
			if regionerror, ok := err.(*raft_storage.RegionError); ok {
				response.RegionError = regionerror.RequestErr
				return response, nil
			}
			return nil, err
		}
		if lock != nil {
			keyerrors = append(keyerrors, &kvrpcpb.KeyError{
				Locked: &kvrpcpb.LockInfo{
					PrimaryLock: lock.Primary,
					LockVersion: lock.Ts,
					Key:         Mutation.Key,
					LockTtl:     lock.Ttl,
				},
			})
			continue
		}
		//写入default和lock
		var kind mvcc.WriteKind
		switch Mutation.Op {
		case kvrpcpb.Op_Put:
			kind = mvcc.WriteKindPut
			txn.PutValue(Mutation.Key, Mutation.Value)
		case kvrpcpb.Op_Del:
			kind = mvcc.WriteKindDelete
			txn.DeleteValue(Mutation.Key)
		case kvrpcpb.Op_Rollback:
			kind = mvcc.WriteKindRollback
		default:
		}
		txn.PutLock(Mutation.Key, &mvcc.Lock{
			Primary: req.PrimaryLock,
			Ts:      req.StartVersion,
			Ttl:     req.LockTtl,
			Kind:    kind,
		})
	}
	if keyerrors != nil {
		response.Errors = keyerrors
		return response, nil
	}
	//不存在冲突则将事务中暂存的修改写入storage
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		if regionerror, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionerror.RequestErr
			return response, nil
		}
		return nil, err
	}
	return response, nil
}

func (server *Server) KvCommit(_ context.Context, req *kvrpcpb.CommitRequest) (*kvrpcpb.CommitResponse, error) {
	// Your Code Here (4B).
	//检查 key 的 lock 的时间戳是否为事务的 startTs，不是直接放弃
	//是则新增一条 Write，删除其对应的 lock
	response := &kvrpcpb.CommitResponse{}
	if req.Keys == nil {
		return response, nil
	}
	//创建reader
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionerror, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionerror.RequestErr
			return response, nil
		}
		return nil, err
	}
	defer reader.Close()
	//创建事务
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	server.Latches.WaitForLatches(req.Keys)
	defer server.Latches.ReleaseLatches(req.Keys)
	for _, key := range req.Keys {
		lock, err := txn.GetLock(key)
		if err != nil {
			if regionerror, ok := err.(*raft_storage.RegionError); ok {
				response.RegionError = regionerror.RequestErr
				return response, nil
			}
			return nil, err
		}
		//已经提交过或者回滚
		if lock == nil {
			// 检查是否存在回滚记录！！！
			write, _, err := txn.CurrentWrite(key)
			if err != nil {
				if regionerror, ok := err.(*raft_storage.RegionError); ok {
					response.RegionError = regionerror.RequestErr
					return response, nil
				}
				return nil, err
			}
			if write != nil && write.Kind == mvcc.WriteKindRollback {
				response.Error = &kvrpcpb.KeyError{
					Abort: "true",
				}
				return response, nil
			}
			return response, nil
		}
		//超时
		if lock.Ts != req.StartVersion {
			response.Error = &kvrpcpb.KeyError{
				Retryable: "true",
			}
			write, _, err := txn.CurrentWrite(key)
			if err != nil {
				if regionerror, ok := err.(*raft_storage.RegionError); ok {
					response.RegionError = regionerror.RequestErr
					return response, nil
				}
				return nil, err
			}
			//判断wirtekind是否为rollback
			if write != nil {
				if write.Kind == mvcc.WriteKindRollback {
					response.Error.Retryable = "false"
				}
			}
			return response, nil
		}
		txn.PutWrite(key, req.CommitVersion, &mvcc.Write{
			StartTS: req.StartVersion,
			Kind:    lock.Kind,
		})
		txn.DeleteLock(key)
	}
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		if regionerror, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionerror.RequestErr
			return response, nil
		}
		return nil, err
	}
	return response, nil
}

func (server *Server) KvScan(_ context.Context, req *kvrpcpb.ScanRequest) (*kvrpcpb.ScanResponse, error) {
	// Your Code Here (4C).
	return nil, nil
}

func (server *Server) KvCheckTxnStatus(_ context.Context, req *kvrpcpb.CheckTxnStatusRequest) (*kvrpcpb.CheckTxnStatusResponse, error) {
	// Your Code Here (4C).
	return nil, nil
}

func (server *Server) KvBatchRollback(_ context.Context, req *kvrpcpb.BatchRollbackRequest) (*kvrpcpb.BatchRollbackResponse, error) {
	// Your Code Here (4C).
	return nil, nil
}

func (server *Server) KvResolveLock(_ context.Context, req *kvrpcpb.ResolveLockRequest) (*kvrpcpb.ResolveLockResponse, error) {
	// Your Code Here (4C).
	return nil, nil
}

// SQL push down commands.
func (server *Server) Coprocessor(_ context.Context, req *coppb.Request) (*coppb.Response, error) {
	resp := new(coppb.Response)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	switch req.Tp {
	case kv.ReqTypeDAG:
		return server.copHandler.HandleCopDAGRequest(reader, req), nil
	case kv.ReqTypeAnalyze:
		return server.copHandler.HandleCopAnalyzeRequest(reader, req), nil
	}
	return nil, nil
}
