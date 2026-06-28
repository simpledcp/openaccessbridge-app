package web

import "embed"

// assetsFS holds the patron + admin static front-end. Embedding keeps the
// appliance dependency-light and fully offline (DECISIONS D4) — no Node build,
// no CDN, no internet.
//
//go:embed assets/*
var assetsFS embed.FS
