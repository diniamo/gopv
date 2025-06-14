//go:build unix

package gopv

import (
	"math/rand"
	"net"
	"os"
	"strconv"
)

func generatePath() (string, error) {
	socketDir := os.TempDir() + "/mpvsockets/"
	err := os.MkdirAll(socketDir, os.ModePerm)
	if err != nil {
		return "", err
	}
	
	return socketDir + strconv.FormatUint(rand.Uint64(), 10), nil
}

func connect(path string, onError func(error)) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}

	return connectInternal(conn, onError), nil
}
