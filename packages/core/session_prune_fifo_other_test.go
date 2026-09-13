//go:build !unix

package core

import "errors"

func makeFifo(string) error {
	return errors.New("unsupported on this platform")
}
