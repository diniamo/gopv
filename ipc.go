package gopv

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net"
	"sync"

	"go.pennock.tech/swallowjson"
)

type ipcResponse struct {
	Error string `json:"error"`
	Data any `json:"data"`
	RequestID int `json:"request_id"`
	Event string `json:"event"`
	EventData map[string]any `json:"-"`
}

type ipcRequest struct {
	Command []any `json:"command"`
	RequestID int `json:"request_id"`
	Async bool `json:"async"`
	responseChan chan *ipcResponse
}

// Represents an IPC client. Cannot be copied.
type Client struct {
	socket net.Conn
	mu sync.Mutex
	receiver chan *ipcRequest
	requests map[int]*ipcRequest
	listeners map[string]func(map[string]any)
	observers map[int]func(any)
	onError func(error)
	// Initially set to true, since this this is for avoiding sending to a closed channel
	// At the start, the channel is open, but callers may have to wait for the sent value to be actually consumed
	closed bool
}

// Represents an error sent by mpv.
type MpvError struct {
	status string
}
func (e MpvError) Error() string {
	return "mpv returned an error: " + e.status
}

// Means that the IPC client is closed (so no requests can be sent).
// Note that the client may be closed without the Close function being called
// (for example if mpv exits).
var ErrClosed = errors.New("IPC client is closed")

// Connects to an active mpv socket, and returns an IPC struct.
// onError may be nil, in which case errors are silently ignored.
func Connect(path string, onError func(error)) (*Client, error) {
	socket, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}

	ipc := &Client{
		socket: socket,
		mu: sync.Mutex{},
		receiver: make(chan *ipcRequest),
		onError: onError,
		closed: false,
	}

	go ipc.write()
	go ipc.read()

	return ipc, nil
}

// Queues an IPC request.
// This is an asynchronous request, which should be the desired behavior most of the time,
// to keep mpv smooth for the user. Asynchronous only refers to how mpv handles the request,
// so the caller is still blocked until mpv returns a response.
// Use RequestSync, if a synchronous request is desired.
func (c *Client) Request(command ...any) (any, error) {
	return c.requestInternal(command, true)
}

// Queues a synchronous IPC request.
func (c *Client) RequestSync(command ...any) (any, error) {
	return c.requestInternal(command, false)
}

// Registers an event listener. If the specified event already has one, it will be overridden.
// The map data received by the listener function may be nil.
// This function already handles enabling the event, so there is no need for another Request call.
func (c *Client) Listen(event string, listener func(map[string]any)) error {
	if c.listeners == nil {
		c.listeners = make(map[string]func(map[string]any), 1)
	} else if _, ok := c.listeners[event]; !ok {
		_, err := c.Request("enable_event", event)
		if err != nil {
			return err
		}
	}

	c.listeners[event] = listener

	return nil
}

// Starts observing a property.
func (c *Client) ObserveProperty(property string, observer func(any)) error {
	id := rand.Int()
	
	_, err := c.Request("observe_property", id, property)
	if err != nil {
		return err
	}

	if c.observers == nil {
		c.observers = make(map[int]func(any), 1)
	}
	c.observers[id] = observer

	return nil
}

// Closes the IPC client. Subsequent requests will fail with ErrClosed.
func (c *Client) Close() {
	c.closed = true

	close(c.receiver)
	c.socket.Close()
}

func (c *Client) write() {
	for {
		req, ok := <-c.receiver
		if !ok {
			return
		}
		
		body, err := json.Marshal(req)
		if err != nil {
			c.publishError(err)
			continue
		}

		c.mu.Lock()
		c.requests[req.RequestID] = req
		c.mu.Unlock()

		// Realistically, this can never fail because the pipe has been closed,
		// since the read loop should immediately exit, and close the request channel.
		_, err = c.socket.Write(body)
		if err != nil {
			c.publishError(err)
			continue
		}
		_, err = c.socket.Write([]byte{'\n'})
		if err != nil {
			c.publishError(err)
			continue
		}
	}
}

func (c *Client) read() {
	reader := bufio.NewReader(c.socket)
	for {
		data, err := reader.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.Close()
				return
			} else {
				c.publishError(err)
				continue
			}
		}

		response := &ipcResponse{}
		err = swallowjson.UnmarshalWith(response, "EventData", data)
		if err != nil {
			c.publishError(err)
			continue
		}

		c.dispatch(response)
	}
}

func (c *Client) requestInternal(command []any, async bool) (any, error) {
	if c.closed {
		return nil, ErrClosed
	}

	if c.requests == nil {
		c.requests = make(map[int]*ipcRequest, 1)
	}

	request := &ipcRequest{
		Command: command,
		RequestID: rand.Int(),
		Async: async,
		responseChan: make(chan *ipcResponse),
	}

	c.receiver <- request
	response := <-request.responseChan
	if response.Error != "success" {
		return nil, &MpvError{response.Error}
	}

	return response.Data, nil	
}

func (c *Client) dispatch(response *ipcResponse) {
	switch response.Event {
	case "":
		c.mu.Lock()
		defer c.mu.Unlock()
	
		request, ok := c.requests[response.RequestID]
		if !ok {
			return
		}

		request.responseChan <- response
		close(request.responseChan)
		delete(c.requests, response.RequestID)
	case "property-change":
		id := int(response.EventData["id"].(float64))
		observer, ok := c.observers[id]
		if ok {
			observer(response.Data)
		}
	default:
		listener, ok := c.listeners[response.Event]
		if ok {
			listener(response.EventData)
		}
	}
}

func (c *Client) publishError(err error) {
	if c.onError != nil {
		c.onError(err)
	}
}
