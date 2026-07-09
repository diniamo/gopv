package gopv

import "os/exec"

// Generates a suitable path for the mpv IPC server.
func GeneratePath() string {
	return generatePath()
}

// Connects to an active mpv IPC server.
// onError may be nil, in which case errors are silently ignored.
func Connect(path string, onError func(error)) (*Client, error) {
	return connect(path, onError)
}

// Sets the passed Cmd up to connect to the returned client as soon as it is started.
// This function does not start the cmd, it only modifies it.
// On Unix, the --input-ipc-client flag is appended to cmd.Args.
// On Windows, the server handle is appended to cmd.SysProcAttr.AdditionalInheritedHandles, and the --input-ipc-client flag is appeneded to cmd.Args.
func Host(cmd *exec.Cmd, onError func(error)) (*Client, error) {
	return host(cmd, onError)
}
