package run

import (
	"bytes"
	"io"
	"strings"
)

func stringReader(s string) io.Reader { return strings.NewReader(s) }
func bytesReader(b []byte) io.Reader  { return bytes.NewReader(b) }
