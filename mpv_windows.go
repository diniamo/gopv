//go:build windows
package gopv

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func generatePath() string {
	return "gopv-" + strconv.Itoa(os.Getpid())
}

func connect(path string, onError func(error)) (*Client, error) {
	handle, err := windows.CreateFile(
		pwstr(path),
		windows.GENERIC_READ | windows.GENERIC_WRITE,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OVERLAPPED | windows.SECURITY_SQOS_PRESENT | windows.SECURITY_ANONYMOUS,
		0,
	)
	if err != nil { return nil, err }

	read, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}

	write, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(read)
		windows.CloseHandle(handle)
		return nil, err
	}

	conn := &pipeConn{
		path:   path,
		handle: handle,
		read:   windows.Overlapped{HEvent: read},
		write:  windows.Overlapped{HEvent: write},
	}
	return connectInternal(conn, onError), nil
}

func host(cmd *exec.Cmd, onError func(error)) (*Client, error) {
	// NOTE: pipe name
	name   := `\\.\pipe\strim-ipc-` + strconv.FormatInt(time.Now().Unix(), 10)
	pwname := pwstr(name)

	// NOTE: create server pipe
	serverSA := windows.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle: 1,
	}
	serverHandle, err := windows.CreateNamedPipe(
		pwname,
		windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE | windows.PIPE_READMODE_BYTE | windows.PIPE_WAIT,
		1,
		4096,
		4096,
		0,
		&serverSA,
	)
	if err != nil { return nil, err }

	// NOTE: connect client pipe
	clientSA := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle:      0,
	}
	clientHandle, err := windows.CreateFile(
		pwname,
		windows.GENERIC_READ | windows.GENERIC_WRITE,
		0,
		&clientSA,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OVERLAPPED | windows.SECURITY_SQOS_PRESENT | windows.SECURITY_ANONYMOUS,
		0,
	)
	if err != nil {
		windows.CloseHandle(serverHandle)
		return nil, err
	}

	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(clientHandle)
		windows.CloseHandle(serverHandle)
		return nil, err
	}

	overlapped := windows.Overlapped{HEvent: event}
	err = windows.ConnectNamedPipe(serverHandle, &overlapped)
	switch err {
	case nil, windows.ERROR_PIPE_CONNECTED:
		break
	case windows.ERROR_IO_INCOMPLETE:
		_, err = windows.WaitForSingleObject(event, windows.INFINITE)
		if err != nil {
			windows.CloseHandle(clientHandle)
			windows.CloseHandle(serverHandle)
			return nil, err
		}
	default:
		windows.CloseHandle(clientHandle)
		windows.CloseHandle(serverHandle)
		return nil, err
	}

	// NOTE: event creation
	write, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(event)
		windows.CloseHandle(clientHandle)
		windows.CloseHandle(serverHandle)
		return nil, err
	}

	// NOTE: cmd setup
	if cmd.SysProcAttr == nil { cmd.SysProcAttr = &syscall.SysProcAttr{} }
	cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, syscall.Handle(serverHandle))

	cmd.Args = append(cmd.Args, "--input-ipc-client=handle://" + strconv.FormatUint(uint64(serverHandle), 10))

	// NOTE: client setup
	conn := &pipeConn{
		path:   name,
		handle: clientHandle,
		read:   windows.Overlapped{HEvent: event},
		write:  windows.Overlapped{HEvent: write},
	}
	client := connectInternal(conn, onError)

	return client, nil
}


func pwstr(s string) *uint16 {
	w, _ := windows.UTF16PtrFromString(s)
	return w
}


type pipeConn struct {
	path   string
	handle windows.Handle
	read   windows.Overlapped
	write  windows.Overlapped
}

func (c *pipeConn) Read(data []byte) (int, error) {
	err := windows.ReadFile(c.handle, data, nil, &c.read)
	if err != nil && err != windows.ERROR_IO_PENDING { return 0, err }

	var n uint32
	err = windows.GetOverlappedResult(c.handle, &c.read, &n, true)
	return int(n), err
}

func (c *pipeConn) Write(data []byte) (int, error) {
	err := windows.WriteFile(c.handle, data, nil, &c.write)
	if err != nil && err != windows.ERROR_IO_PENDING { return 0, err }

	var n uint32
	err = windows.GetOverlappedResult(c.handle, &c.write, &n, true)
	return int(n), err
}

func (c *pipeConn) Close() error {
	windows.CloseHandle(c.write.HEvent)
	windows.CloseHandle(c.read.HEvent)
	return windows.CloseHandle(c.handle)
}

func (c *pipeConn) LocalAddr()  net.Addr { return pipeAddr(c.path) }
func (c *pipeConn) RemoteAddr() net.Addr { return pipeAddr(c.path) }

var errDeadlineUnsupported = errors.New("deadline not supported")
func (c *pipeConn) SetDeadline(t time.Time)      error { return errDeadlineUnsupported }
func (c *pipeConn) SetReadDeadline(t time.Time)  error { return errDeadlineUnsupported }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return errDeadlineUnsupported }

type pipeAddr string
func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String()  string { return string(a) }
