package transform

import (
	"strings"
	"testing"
)

func TestStringAccumulatorSupportsRepeatedOutputAndFurtherWrites(t *testing.T) {
	var accumulator StringAccumulator
	if accumulator.String() != "" || accumulator.Len() != 0 {
		t.Fatal("zero accumulator should be empty")
	}

	for index := range inlineStringFragments + 3 {
		accumulator.WriteString(string(rune('a' + index)))
	}
	accumulator.WriteString("")
	want := "abcdefghijk"
	if got := accumulator.String(); got != want {
		t.Fatalf("first String()=%q, want %q", got, want)
	}
	if got := accumulator.String(); got != want {
		t.Fatalf("repeated String()=%q, want %q", got, want)
	}
	if accumulator.Len() != len(want) {
		t.Fatalf("Len()=%d, want %d", accumulator.Len(), len(want))
	}

	accumulator.WriteString("-tail")
	if got := accumulator.String(); got != want+"-tail" {
		t.Fatalf("String() after further write=%q", got)
	}

	accumulator.Reset()
	if accumulator.String() != "" || accumulator.Len() != 0 {
		t.Fatal("Reset() should clear accumulated content")
	}
}

func TestStringAccumulatorKeepsSingleFragmentWithoutJoining(t *testing.T) {
	value := strings.Repeat("x", 1024)
	var accumulator StringAccumulator
	accumulator.WriteString(value)
	if got := accumulator.String(); got != value {
		t.Fatalf("String()=%q, want original fragment", got)
	}
}

func TestStringAccumulatorCrossesStorageBlocks(t *testing.T) {
	const fragmentCount = inlineStringFragments + 3
	fragment := strings.Repeat("block-boundary", 512)
	var accumulator StringAccumulator
	for range fragmentCount {
		accumulator.WriteString(fragment)
	}
	want := strings.Repeat(fragment, fragmentCount)
	var builder strings.Builder
	builder.WriteString("prefix:")
	accumulator.AppendTo(&builder)
	if got := builder.String(); got != "prefix:"+want {
		t.Fatalf("AppendTo result length=%d, want %d", len(got), len("prefix:")+len(want))
	}
	if got := accumulator.String(); got != want {
		t.Fatalf("cross-block result length=%d, want %d", len(got), len(want))
	}
}

func TestStringAccumulatorBlockPoolBoundsAndResets(t *testing.T) {
	for size := minStringBlockSize; size <= maxStringBlockSize; size *= 2 {
		index, ok := stringBlockPoolIndex(size)
		if !ok || index < 0 || index >= len(stringBlockPools) {
			t.Fatalf("pool index for %d bytes = %d, %v", size, index, ok)
		}
		block := acquireStringAccumulatorBlock(size)
		block.data = append(block.data, "sensitive-data"...)
		releaseStringAccumulatorBlock(block)
		reused := acquireStringAccumulatorBlock(size)
		if len(reused.data) != 0 || cap(reused.data) != size {
			t.Fatalf("reused %d-byte block has len=%d cap=%d", size, len(reused.data), cap(reused.data))
		}
		reused.data = reused.data[:len("sensitive-data")]
		if strings.Trim(string(reused.data), "\x00") != "" {
			t.Fatalf("reused %d-byte block retained previous content", size)
		}
		releaseStringAccumulatorBlock(reused)
	}

	for _, size := range []int{0, minStringBlockSize - 1, maxStringBlockSize + 1, 768} {
		if _, ok := stringBlockPoolIndex(size); ok {
			t.Fatalf("unexpected pool class for size %d", size)
		}
	}

	var accumulator StringAccumulator
	for range 1024 {
		accumulator.WriteString(strings.Repeat("x", 32))
	}
	accumulator.Reset()
	if len(accumulator.blocks) != 0 || accumulator.String() != "" || accumulator.Len() != 0 {
		t.Fatal("Reset() should release pooled blocks and clear state")
	}
}
