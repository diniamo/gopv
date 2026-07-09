//go:build unix
package gopv

import (
	"net"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"
)

func generatePath() string {
	return "/tmp/gopv-" + strconv.Itoa(os.Getpid()) + ".sock"
}

func connect(path string, onError func(error)) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}

	return connectInternal(conn, onError), nil
}

func host(cmd *exec.Cmd, onError func(error)) (*Client, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil { return nil, err }

	file      := os.NewFile(uintptr(fds[0]), "")
	conn, err := net.FileConn(file)
	if err != nil { return nil, err }

	client := connectInternal(conn, onError)
	cmd.Args = append(cmd.Args, "--input-ipc-client=fd://" + strconv.Itoa(fds[1]))

	return client, nil
}
