//go:build windows

package gopv

import (
	"math/rand"
	"strconv"

	"github.com/Microsoft/go-winio"
)

func generatePath() (string, error) {
	return `\\.\pipe\` + strconv.FormatUint(rand.Uint64(), 10), nil
}

func connect(path string, onError func(error)) (*Client, error) {
	conn, err := winio.DialPipe(path, nil)
	if err != nil {
		return nil, err
	}

	return connectInternal(conn, onError), nil
}
