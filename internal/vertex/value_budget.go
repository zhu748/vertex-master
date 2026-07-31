package vertex

import "github.com/bsfdsagfadg/vertex/internal/jsonx"

// valueBudgetMaxDepth 防止异常深的动态 JSON 结构让预算遍历消耗过多栈空间。
// 超限时按“不适合并行复制/缓存”处理，比继续递归更安全。
const valueBudgetMaxDepth = 256

type canonicalTextContent interface {
	CanonicalTextContent() (role, text string, ok bool)
}

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
	case canonicalTextContent:
		role, text, ok := typed.CanonicalTextContent()
		if !ok || !consumeCanonicalTextContentBudget(remaining, role, text) {
			return false
		}
	case jsonx.CanonicalObjectView:
		fieldCount, ok := typed.CanonicalJSONFieldCount()
		if !ok || fieldCount > *remaining {
			return false
		}
		for index := range fieldCount {
			key, item := typed.CanonicalJSONField(index)
			*remaining -= len(key)
			if !valueFitsBudgetDepth(item, remaining, depth+1) {
				return false
			}
		}
	case jsonx.CanonicalArrayView:
		itemCount, ok := typed.CanonicalJSONItemCount()
		if !ok || itemCount > *remaining {
			return false
		}
		for index := range itemCount {
			if !valueFitsBudgetDepth(typed.CanonicalJSONItem(index), remaining, depth+1) {
				return false
			}
		}
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

func consumeCanonicalTextContentBudget(remaining *int, role, text string) bool {
	// Additional cost after the outer value node: top-level "parts"/"role"
	// keys, one-element parts array, nested text object/key, and both scalar
	// nodes. This exactly matches the dynamic map representation.
	*remaining -= 17
	if *remaining < 0 {
		return false
	}
	*remaining -= len(role)
	if *remaining < 0 {
		return false
	}
	*remaining -= len(text)
	return *remaining >= 0
}
