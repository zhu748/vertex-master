package transform

import "strings"

const (
	inlineStringFragments = 8
	initialStringBlocks   = 4
	minStringBlockSize    = 256
	maxStringBlockSize    = 16 * 1024
	stringBlockPoolDepth  = 32
)

type stringAccumulatorBlock struct {
	data []byte
}

// The accumulator only uses these seven power-of-two block sizes. Bounded
// channels keep reuse predictable: a burst can retain at most about 1 MiB,
// while overflow blocks remain eligible for normal garbage collection.
var stringBlockPools = [...]chan *stringAccumulatorBlock{ //nolint:gochecknoglobals
	make(chan *stringAccumulatorBlock, stringBlockPoolDepth), // 256 B
	make(chan *stringAccumulatorBlock, stringBlockPoolDepth), // 512 B
	make(chan *stringAccumulatorBlock, stringBlockPoolDepth), // 1 KiB
	make(chan *stringAccumulatorBlock, stringBlockPoolDepth), // 2 KiB
	make(chan *stringAccumulatorBlock, stringBlockPoolDepth), // 4 KiB
	make(chan *stringAccumulatorBlock, stringBlockPoolDepth), // 8 KiB
	make(chan *stringAccumulatorBlock, stringBlockPoolDepth), // 16 KiB
}

// StringAccumulator 累积流式字符串片段，并在首次 String 时一次性精确合并。
// 短响应直接保存在内联槽位中；合并后会释放原始片段并缓存结果，因此重复读取
// 不会再次分配，读取后继续写入也仍会保留已有内容。
type StringAccumulator struct {
	inline        [inlineStringFragments]string
	blocks        []*stringAccumulatorBlock
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
	a.releaseBlocks()
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
		builder.Write(block.data)
	}
}

func (a *StringAccumulator) startBlocks(required int) {
	a.blocks = make([]*stringAccumulatorBlock, 0, initialStringBlocks)
	size := minStringBlockSize
	for size < required && size < maxStringBlockSize {
		size *= 2
	}
	a.nextBlockSize = min(size, maxStringBlockSize)
}

func (a *StringAccumulator) writeToBlocks(value string) {
	for len(value) > 0 {
		if len(a.blocks) == 0 || len(a.blocks[len(a.blocks)-1].data) == cap(a.blocks[len(a.blocks)-1].data) {
			size := a.nextBlockSize
			if size == 0 {
				size = minStringBlockSize
			}
			a.blocks = append(a.blocks, acquireStringAccumulatorBlock(size))
			a.nextBlockSize = min(size*2, maxStringBlockSize)
		}

		last := len(a.blocks) - 1
		available := cap(a.blocks[last].data) - len(a.blocks[last].data)
		written := min(len(value), available)
		a.blocks[last].data = append(a.blocks[last].data, value[:written]...)
		value = value[written:]
	}
}

func acquireStringAccumulatorBlock(size int) *stringAccumulatorBlock {
	index, ok := stringBlockPoolIndex(size)
	if ok {
		select {
		case block := <-stringBlockPools[index]:
			block.data = block.data[:0]
			return block
		default:
		}
	}
	return &stringAccumulatorBlock{data: make([]byte, 0, size)}
}

func releaseStringAccumulatorBlock(block *stringAccumulatorBlock) {
	if block == nil {
		return
	}
	index, ok := stringBlockPoolIndex(cap(block.data))
	if !ok {
		return
	}
	clear(block.data)
	block.data = block.data[:0]
	select {
	case stringBlockPools[index] <- block:
	default:
	}
}

func stringBlockPoolIndex(size int) (int, bool) {
	if size < minStringBlockSize || size > maxStringBlockSize || size&(size-1) != 0 {
		return 0, false
	}
	index := 0
	for size > minStringBlockSize {
		size >>= 1
		index++
	}
	return index, index < len(stringBlockPools)
}

func (a *StringAccumulator) releaseBlocks() {
	for _, block := range a.blocks {
		releaseStringAccumulatorBlock(block)
	}
	a.blocks = nil
}

// Reset 丢弃已累积的内容。
func (a *StringAccumulator) Reset() {
	if a != nil {
		a.releaseBlocks()
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
