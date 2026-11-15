//go:build !unix

package main

import (
	"errors"
	"io"
)

// detach needs a named pipe, which only Unix systems have.
func detach(dir string) (io.Reader, io.Writer, func(), error) {
	return nil, nil, nil, errors.New("--detached needs a Unix system")
}
