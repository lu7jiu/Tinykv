package server

import (
	"context"

	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
)

// The functions below are Server's Raw API. (implements TinyKvServer).
// Some helper methods can be found in sever.go in the current directory

// RawGet return the corresponding Get response based on RawGetRequest's CF and Key fields
func (server *Server) RawGet(_ context.Context, req *kvrpcpb.RawGetRequest) (*kvrpcpb.RawGetResponse, error) {
	// Your Code Here (1).
	storageread, err := server.storage.Reader(req.Context)
	if err != nil {
		return &kvrpcpb.RawGetResponse{Error: err.Error()}, err
	}
	defer storageread.Close()
	val, err := storageread.GetCF(req.Cf, req.Key)
	//key不存在error为nil，NotFound为true
	//if err!=nil{
	//	return &kvrpcpb.RawGetResponse{Error:err.Error()},err
	//}
	rawgetresponse := &kvrpcpb.RawGetResponse{
		Value:    val,
		NotFound: false,
	}
	if val == nil {
		rawgetresponse.NotFound = true
	}
	return rawgetresponse, nil
}

// RawPut puts the target data into storage and returns the corresponding response
func (server *Server) RawPut(_ context.Context, req *kvrpcpb.RawPutRequest) (*kvrpcpb.RawPutResponse, error) {
	// Your Code Here (1).
	// Hint: Consider using Storage.Modify to store data to be modified
	var batch []storage.Modify
	batch = append(batch, storage.Modify{
		Data: storage.Put{
			Key:   req.Key,
			Value: req.Value,
			Cf:    req.Cf,
		},
	})
	err := server.storage.Write(req.Context, batch)
	if err != nil {
		return &kvrpcpb.RawPutResponse{Error: err.Error()}, nil
	}
	return &kvrpcpb.RawPutResponse{}, nil
}

// RawDelete delete the target data from storage and returns the corresponding response
func (server *Server) RawDelete(_ context.Context, req *kvrpcpb.RawDeleteRequest) (*kvrpcpb.RawDeleteResponse, error) {
	// Your Code Here (1).
	// Hint: Consider using Storage.Modify to store data to be deleted
	var batch []storage.Modify
	batch = append(batch, storage.Modify{
		Data: storage.Delete{
			Key: req.Key,
			Cf:  req.Cf,
		},
	})
	err := server.storage.Write(req.Context, batch)
	if err != nil {
		return &kvrpcpb.RawDeleteResponse{Error: err.Error()}, nil
	}
	return &kvrpcpb.RawDeleteResponse{}, nil
}

// RawScan scan the data starting from the start key up to limit. and return the corresponding result
func (server *Server) RawScan(_ context.Context, req *kvrpcpb.RawScanRequest) (*kvrpcpb.RawScanResponse, error) {
	// Your Code Here (1).
	// Hint: Consider using reader.IterCF
	storageread, err := server.storage.Reader(req.Context)
	if err != nil {
		return &kvrpcpb.RawScanResponse{Error: err.Error()}, err
	}
	defer storageread.Close()
	iterator := storageread.IterCF(req.Cf)
	defer iterator.Close()
	iterator.Seek(req.StartKey)
	var kvs []*kvrpcpb.KvPair
	for i := req.Limit; iterator.Valid(); iterator.Next() {
		if i == 0 {
			break
		}
		item := iterator.Item()
		val, err := item.ValueCopy(nil)
		if err != nil {
			return &kvrpcpb.RawScanResponse{Error: err.Error()}, err
		}
		kvpair := &kvrpcpb.KvPair{
			Key:   item.KeyCopy(nil),
			Value: val,
		}
		kvs = append(kvs, kvpair)
		i--
	}
	return &kvrpcpb.RawScanResponse{Kvs: kvs}, nil
}
