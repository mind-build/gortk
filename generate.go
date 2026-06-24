package gortk

// Regenerate specs/rtk.json from upstream rtk's TOML filter catalog. rtksync
// lives in the sibling rtkcompat module (which owns the TOML dependency), so the
// directive runs it from there. Run with `go generate ./...`.
//
//go:generate sh -c "cd rtkcompat && go run ./cmd/rtksync"
