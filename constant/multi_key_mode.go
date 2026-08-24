package constant

type MultiKeyMode string

const (
	MultiKeyModeRandom     MultiKeyMode = "random"     // 随机
	MultiKeyModePolling    MultiKeyMode = "polling"    // 轮询
	MultiKeyModeSequential MultiKeyMode = "sequential"  // 顺序(优先):始终使用第一个可用 key,失败后切换下一个
)
