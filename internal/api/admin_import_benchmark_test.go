package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/nodes"
)

var benchmarkImportedNodes []nodes.Node //nolint:gochecknoglobals

func BenchmarkParseImportedNodesThousand(b *testing.B) {
	var uriList strings.Builder
	jsonItems := make([]map[string]any, 1000)
	for index := range 1000 {
		_, _ = fmt.Fprintf(
			&uriList,
			"http://user:pass@proxy-%d.example.com:8080#node-%d\n",
			index,
			index,
		)
		jsonItems[index] = map[string]any{
			"type": "http", "name": fmt.Sprintf("node-%d", index),
			"server": fmt.Sprintf("proxy-%d.example.com", index), "port": 8080,
		}
	}
	jsonList, err := json.Marshal(jsonItems)
	if err != nil {
		b.Fatal(err)
	}

	for name, input := range map[string]string{
		"uri_list":  uriList.String(),
		"json_list": string(jsonList),
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			for range b.N {
				benchmarkImportedNodes = parseImportedNodes(input)
				if len(benchmarkImportedNodes) != 1000 {
					b.Fatalf("imported nodes=%d", len(benchmarkImportedNodes))
				}
			}
		})
	}
}
