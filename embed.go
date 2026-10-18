// Package squark embeds the squark runtime and standard library so the compiler binary is self-contained.
package squark

import "embed"

//go:embed runtime std
var Files embed.FS

// Tour is the squark tour (squark tour): its web page and lessons.
//
//go:embed tour
var Tour embed.FS
