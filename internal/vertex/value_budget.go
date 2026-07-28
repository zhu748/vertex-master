package vertex

// valueBudgetMaxDepth 防止异常深的动态 JSON 结构让预算遍历消耗过多栈空间。
// 超限时按“不适合并行复制/缓存”处理，比继续递归更安全。
const valueBudgetMaxDepth = 256

func valueFitsBudget(value any, remaining *int) bool {
	return valueFitsBudgetDepth(value, remaining, 0)
}

func valueFitsBudgetDepth(value any, remaining *int, depth int) bool {
	if depth > valueBudgetMaxDepth {
		return false
	}
	// 每个结构节点也消耗一点预算，避免由海量数字/布尔值组成、但字符串很少
	// 的动态 JSON 绕过大小限制。
	*remaining--
	if *remaining < 0 {
		return false
	}
	switch typed := value.(type) {
	case string:
		*remaining -= len(typed)
	case []byte:
		*remaining -= len(typed)
	case []any:
		for _, item := range typed {
			if !valueFitsBudgetDepth(item, remaining, depth+1) {
				return false
			}
		}
	case map[string]any:
		for key, item := range typed {
			*remaining -= len(key)
			if !valueFitsBudgetDepth(item, remaining, depth+1) {
				return false
			}
		}
	}
	return *remaining >= 0
}
