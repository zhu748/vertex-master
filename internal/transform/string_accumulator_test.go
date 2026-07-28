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
