module workbuddy2api

go 1.26

require (
	github.com/redis/go-redis/v9 v9.18.0
	// workbuddy2api-gui 面板（git subtree，见 panel/）。
	// 它是独立 module（无外部依赖）：保持原 module 路径 + replace 指向本地目录，
	// 面板内的 import 一行都不用改，今后 git subtree pull 才不会必然冲突。
	workbuddy2api-gui v0.0.0
)

replace workbuddy2api-gui => ./panel

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	go.uber.org/atomic v1.11.0 // indirect
)
