package transform

import "strings"

const (
	inlineStringFragments = 8
	minStringBlockSize    = 256
	maxStringBlockSize    = 16 * 1024
)

// StringAccumulator 累积流式字符串片段，并在首次 String 时一次性精确合并。
// 短响应直接保存在内联槽位中；合并后会释放原始片段并缓存结果，因此重复读取
// 不会再次分配，读取后继续写入也仍会保留已有内容。
type StringAccumulator struct {
	inline        [inlineStringFragments]string
	blocks        [][]byte
	count         int
	length        int
	nextBlockSize int
}

// WriteString 追加一个非空字符串片段。
func (a *StringAccumulator) WriteString(value string) {
	if a == nil || value == "" {
		return
	}
	if a.count < len(a.inline) {
		a.inline[a.count] = value
	} else {
		if a.blocks == nil {
			a.startBlocks(a.length + len(value))
			for _, fragment := range a.inline {
				a.writeToBlocks(fragment)
			}
			clear(a.inline[:])
		}
		a.writeToBlocks(value)
	}
	a.count++
	a.length += len(value)
}

// String 返回当前完整内容。首次合并后仅保留最终字符串，以免响应结束前继续
// 持有所有上游 chunk；后续调用直接返回缓存值。
func (a *StringAccumulator) String() string {
	if a == nil || a.count == 0 {
		return ""
	}
	if a.count == 1 {
		return a.inline[0]
	}

	var builder strings.Builder
	builder.Grow(a.length)
	a.AppendTo(&builder)
	value := builder.String()

	clear(a.inline[:])
	a.inline[0] = value
	a.blocks = nil
	a.count = 1
	a.length = len(value)
	a.nextBlockSize = 0
	return value
}

// AppendTo 把当前内容追加到目标 Builder，不合并或重置累积器。调用方已知最终
// 总长度时可直接构造外层结果，避免先生成中间字符串。
func (a *StringAccumulator) AppendTo(builder *strings.Builder) {
	if a == nil || builder == nil {
		return
	}
	if a.blocks == nil {
		for index := 0; index < min(a.count, len(a.inline)); index++ {
			builder.WriteString(a.inline[index])
		}
		return
	}
	for _, block := range a.blocks {
		builder.Write(block)
	}
}

func (a *StringAccumulator) startBlocks(required int) {
	size := minStringBlockSize
	for size < required && size < maxStringBlockSize {
		size *= 2
	}
	a.nextBlockSize = min(size, maxStringBlockSize)
}

func (a *StringAccumulator) writeToBlocks(value string) {
	for len(value) > 0 {
		if len(a.blocks) == 0 || len(a.blocks[len(a.blocks)-1]) == cap(a.blocks[len(a.blocks)-1]) {
			size := a.nextBlockSize
			if size == 0 {
				size = minStringBlockSize
			}
			a.blocks = append(a.blocks, make([]byte, 0, size))
			a.nextBlockSize = min(size*2, maxStringBlockSize)
		}

		last := len(a.blocks) - 1
		available := cap(a.blocks[last]) - len(a.blocks[last])
		written := min(len(value), available)
		a.blocks[last] = append(a.blocks[last], value[:written]...)
		value = value[written:]
	}
}

// Reset 丢弃已累积的内容。
func (a *StringAccumulator) Reset() {
	if a != nil {
		*a = StringAccumulator{}
	}
}

// Len 返回当前累计字节数。
func (a *StringAccumulator) Len() int {
	if a == nil {
		return 0
	}
	return a.length
}
