# Project1 StandaloneKV

构建一个支持列族（column family）的独立键值存储 gRPC 服务。该服务支持四种基本操作：Put（插入）/Delete（删除）/Get（获取）/Scan（扫描）。

### 实现独立存储引擎

#### txn

txn是badger的事务，对badger进行获取、插入、删除等操作都可以借助txn来完成。

#### engine_util 

该包对 badger 进行了包装，通过给键添加前缀cf来模拟列族，以提供带列族的操作，同时也提供了许多有用的辅助函数。

1.engines.go:定义了`Engines`结构体及其方法，包括数据写入，关闭、删除`Engines`，以及创建`Engines`、`DB`的函数。

2.util.go:提供了对`badger.DB`的封装操作，包括插入、删除、读取等，支持列族操作与事务。

3.write_batch.go：实现了批量写入操作。

4.cf_iterator.go：添加cf前缀，实现了`DBIterator`、`DBItem`两个接口。

#### 独立存储引擎实现过程

`Storage`接口中的`Reader`方法返回一个`StorageReader`，因此先实现`StorageReader`接口。`Reader`使用`badger.Txn`来实现，因此定义结构体`StandAloneStorageReader`包含txn即可。
- `NewStandAloneStorageReader`：创建一个`StandAloneStorageReader`实例。
- `GetCF`：调用`engine_util.GetCFFromTxn`实现，返回cf_key对应的value。
- `IterCF`：调用`engine_util.NewCFIterator`实现，返回列族感知的迭代器。
- `Close`：调用`Discard`关闭事务。

实现`Storage`接口，首先需定义`StandAloneStorage`结构体，应包含`badger.DB`的实例以及创建该数据库的参数`config.Config`。
- `NewStandAloneStorage`：调用`engine_util.CreateDB`创建DB，返回包含该DB和Config的`StandAloneStorage`实例。
- `Stop`:调用`Close`关闭数据库。
- `Reader`：调用`NewTransaction(false)`创建只读事务，返回以该txn为参数创建的`StandAloneStorageReader`实例。
- `Write`：使用`engine_util.WriteBatch`的批量写入方法，根据`batch`中`Modify.Data`的类型来判断操作类型，如果是`storage.Put`类型则调用`SetCF`添加写入操作，如果是`storage.Delete`类型则调用`DeleteCF`添加删除操作，最后调用`WriteToDB`批量写入。

### 实现原始键值服务处理器

查看proto/pkg/kvrpcpb/kvrpcpb.pb.go文件中各Request和Response的结构体定义，明确参数内容。

- `RawGet`：借助`GetCF`方法完成。
- `RawScan`：借助`IterCF`方法完成，使用`Seek`定位到`StartKey`的数据项，向后继续扫描。
- `RawPut`：借助`Write`方法，使用`Modify`存放待插入数据，对应Data类型为`storage.Put`。
- `RawDelete`：同上，Data类型为`storage.Delete`。

### 问题及解决

TestRawDelete1测试失败，通过查看测试程序发现删除数据后调用`Get`函数查找被删除的数据，此时希望err为nil，而实际返回err为Key not found。而`Get`是通过`GetCF`实现的，于是修改`GetCF`，当调用`engine_util.GetCFFromTxn`返回的`err==badger.ErrKeyNotFound`时，`GetCF`返回错误为nil。
