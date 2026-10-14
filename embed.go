// Package squark embeds the squark runtime and standard library so the compiler binary is self-contained.
package squark

import "embed"

//go:embed runtime std
var Files embed.FS
