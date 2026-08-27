package webui

import (
	"embed"
	"io/fs"
)

// fsSub narrows an embedded filesystem to a subdirectory, so URLs do not carry
// the internal directory name.
func fsSub(embedded embed.FS, dir string) (fs.FS, error) { return fs.Sub(embedded, dir) }
