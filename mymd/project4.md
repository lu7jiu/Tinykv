# project4 Transaction

### 4a 实现 MVCC 

用户键和时间戳组合成一个编码键。键的编码方式使得编码键的升序首先按用户键（升序）排序，然后按时间戳（降序）排序。这确保迭代编码键时会首先得到最新版本。transaction.go 中定义了用于编码和解码键的辅助函数。

TinyKV 使用三个列簇（CF）：default 用于保存用户值，lock 用于存储锁，write 用于记录更改。

```go
const (
	CfDefault string = "default"
	CfWrite   string = "write"
	CfLock    string = "lock"
)

var CFs [3]string = [3]string{CfDefault, CfWrite, CfLock}
```

在进行具体实现之前，需要了解三个列簇的结构
1.Lock

![Lock结构图片](https://picx.zhimg.com/v2-40753cc9132dabc2f534bcd5aca59def_1440w.jpg "Lock结构")

Lock 的 Key 仅仅由 Cf_Lock 和源 Key 拼接而成，不含 Ts 信息。Lock 的 Ts 信息同 Ttl、Kind、Primary Key 一并存在 Value 中。

2.Write

![Write结构图片](https://pica.zhimg.com/v2-36d8868b54aa83f47412a0122b19fec2_1440w.jpg "Write结构")

不同于 Lock，Write 的 Key 中是整合了 commitTs 的，首先通过 EncodeKey 将源 Key 和 commitTs 编码在一起，然后和 Cf_Write 拼接形成新的 Key。Write 的 StartTs 同 Kind 一并存在 Value 中。

3.Default

![Default结构图片](https://pic4.zhimg.com/v2-fa9546e5f58cd000262e916d3e50f5cd_1440w.jpg "Default结构")

不同于 Write，Default 的 Key 中整合的是 startTs，而不是 commitTs，用于 Write 进行索引，写入的值存在 Value 中即可。
