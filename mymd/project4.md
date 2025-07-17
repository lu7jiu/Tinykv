# project4 Transaction

### 快照隔离

事务承诺提供快照隔离。这意味着在一个事务中，客户端读取数据库时，就好像数据库在事务开始时被冻结了一样（事务看到的是数据库的一个一致视图）。一个事务的所有操作要么全部写入数据库，要么全部不写入（如果它与另一个事务冲突）。

### 两阶段提交协议（2PC）

TinyKV 的事务设计遵循两阶段提交协议。
1.预写（Prewrite）：若无写冲突，锁定键、添加value记录
2.提交（Commit）：核实锁为当前事务的锁，添加写入记录、移除锁

### 4a 实现 MVCC 

为了提供快照隔离，需要改变数据在底层存储中的存储方式。不再为每个键存储一个值，而是为一个键和一个时间（由时间戳表示）存储一个值。这被称为多版本并发控制（MVCC），因为每个键都存储了值的多个不同版本。

用户键和时间戳组合成一个编码键。键的编码方式使得编码键的升序首先按用户键（升序）排序，然后按时间戳（降序）排序。这确保迭代编码键时会首先得到最新版本。transaction.go 中定义了用于编码和解码键的辅助函数。

TinyKV 使用三个列簇（CF）：default 用于保存用户值，lock 用于存储锁，write 用于记录更改。

```go
const (
	CfDefault string = "default"
	CfWrite   string = "write"
	CfLock    string = "lock"
)
```

在进行具体实现之前，需要了解三个列簇的结构

1.Lock

![Lock结构图片](https://github.com/sakura-ysy/TinyKV-2022-doc/raw/main/doc/project4/%E5%9B%BE%E7%89%872.png "Lock结构")

Lock 的 Key 仅仅由 Cf_Lock 和原 Key 拼接而成，不含 Ts 信息。Lock 的 Ts 信息同 Ttl、Kind、Primary Key 一并存在 Value 中。

2.Write

![Write结构图片](https://github.com/sakura-ysy/TinyKV-2022-doc/raw/main/doc/project4/%E5%9B%BE%E7%89%875.png "Write结构")

Write 的 Key 中添加了 commitTs ，首先通过 EncodeKey 将原 Key 和 commitTs 编码在一起，然后和 Cf_Write 拼接形成新的 Key。Write 的 StartTs 同 Kind 一并存在 Value 中。

3.Default

![Default结构图片](https://github.com/sakura-ysy/TinyKV-2022-doc/raw/main/doc/project4/%E5%9B%BE%E7%89%877.png "Default结构")

Default 的 Key 中添加的是 startTs，而不是 commitTs，用于 Write 进行索引，写入的值存在 Value 中即可。

### 4b、4c 实现事务API

#### 1.KvGet

1.检查要读取的键是否被另一个事务锁定，是则表示有未提交的修改，可能产生脏读，返回错误
2.读取事务开始时间戳之前最新提交的数据

#### 2.KvPrewrite

1.检查是否存在另一个事务锁定或写入同一个键，若存在则有写冲突，记录并返回错误
2.若无冲突，则添加lock和default记录来锁定并存储值

#### 3.KvCommit

1.检查键是否被当前事务锁定（锁的时间戳是否为当前事务的开始时间戳），若不是则返回错误
2.是则添加write记录表示值已提交，并且解锁

#### 4.KvScan

1.实现扫描器

- `Next`方法中更新nextkey时是在所有数据中寻找下一个不同的key作为下一个nextkey，可能会找到一个在当前事务中不可见的记录的key，因此需要进行检查，若按照nextkey实际读取的key不为nextkey，则需要修正nextkey为实际读取的key

2.使用扫描器查询数据，检查要读取的键不被另一个事务锁定，才能读取数据

#### 5.KvCheckTxnStatus

事务的状态及判断方法
1.提交：存在一个write记录且`write.Kind != WriteKindRollback`
2.回滚：lock为空
3.超时：lock存在且`PhysicalTime(lock.Ts)+lock.Ttl <= PhysicalTime(req.CurrentTs)`
4.正常：上述情况以外的状态

#### 6.KvBatchRollback

和`KvCommit`相反
1.检查键是否被当前事务锁定，若事务已提交则终止
2.是则移除该锁，删除所有相关值，并留下一个回滚标识作为写入记录

#### 7.KvResolveLock

1.根据开始时间戳找到在事务中有锁的key
2.根据CommitVersion判断批量提交或批量回滚，调用相应函数