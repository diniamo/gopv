package gopv

// Generates a suitable path for the mpv IPC server
func GeneratePath() (string, error) {
	return generatePath()
}

// Connects to an active mpv IPC server, and returns a Client struct representing it.
// onError may be nil, in which case errors are silently ignored.
func Connect(path string, onError func(error)) (*Client, error) {
	return connect(path, onError)
}
