package api

import "net/http/pprof"

var (
	//nolint:gochecknoglobals // Function references for routing
	pprofIndex = pprof.Index
	//nolint:gochecknoglobals // Function references for routing
	pprintCmdline = pprof.Cmdline
	//nolint:gochecknoglobals // Function references for routing
	pprofProfile = pprof.Profile
	//nolint:gochecknoglobals // Function references for routing
	pprofSymbol = pprof.Symbol
	//nolint:gochecknoglobals // Function references for routing
	pprofTrace = pprof.Trace
	//nolint:gochecknoglobals // Function references for routing
	pprofGoroutine = pprof.Handler("goroutine").ServeHTTP
	//nolint:gochecknoglobals // Function references for routing
	pprofHeap = pprof.Handler("heap").ServeHTTP
	//nolint:gochecknoglobals // Function references for routing
	pprofThreadcreate = pprof.Handler("threadcreate").ServeHTTP
	//nolint:gochecknoglobals // Function references for routing
	pprofBlock = pprof.Handler("block").ServeHTTP
	//nolint:gochecknoglobals // Function references for routing
	pprofMutex = pprof.Handler("mutex").ServeHTTP
)
