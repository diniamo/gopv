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
	ID int `json:"id"`
}

type ipcRequest struct {
	Command []any `json:"command"`
	RequestID int `json:"request_id"`
	Async bool `json:"async"`
	responseChan chan *ipcResponse
}

// Represents an IPC client. Cannot be copied.
type Client struct {
	receiver chan *ipcRequest
	conn net.Conn
	
	requestMu sync.Mutex
	requests map[int]*ipcRequest

	listenerMu sync.Mutex
	listeners map[string]func(map[string]any)

	observerMu sync.Mutex
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

func connectInternal(conn net.Conn, onError func(error)) *Client {
	client := &Client{
		receiver: make(chan *ipcRequest),
		conn: conn,
		requestMu: sync.Mutex{},
		listenerMu: sync.Mutex{},
		observerMu: sync.Mutex{},
		onError: onError,
		closed: false,
	}

	go client.write()
	go client.read()

	return client
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

// Queues a request parsed from JSON.
// A custom request id is added before the request is sent.
func (c *Client) RequestJSON(requestRaw []byte) (any, error) {
	request := &ipcRequest{}
	err := json.Unmarshal(requestRaw, request)
	if err != nil {
		return nil, err
	}

	request.RequestID = rand.Int()
	request.responseChan = make(chan *ipcResponse, 1)

	return c.requestSend(request)
}

// Registers an event listener. If the specified event already has one, it will be overridden.
// The listener function is always run in a new goroutine.
// The map data received by the listener function may be nil.
// This function already handles enabling the event, so there is no need for another Request call.
func (c *Client) RegisterListener(event string, listener func(map[string]any)) error {
	c.listenerMu.Lock()
	defer c.listenerMu.Unlock()

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

// Unregisters the event listener associated with an event.
// Attempts a disable_event request, but fails silently.
func (c *Client) UnregisterListener(event string) {
	c.listenerMu.Lock()
	delete(c.listeners, event)
	c.listenerMu.Unlock()

	go c.Request("disable_event", event)
}

// Starts observing a property.
// The observer function is always run in a new goroutine.
// The returned integer is the observation id, which can be passed to UnobserveProperty later.
func (c *Client) ObserveProperty(property string, observer func(any)) (int, error) {
	id := rand.Int()
	
	_, err := c.Request("observe_property", id, property)
	if err != nil {
		return 0, err
	}

	c.observerMu.Lock()
	if c.observers == nil {
		c.observers = make(map[int]func(any), 1)
	}
	
	c.observers[id] = observer
	c.observerMu.Unlock()

	return id, err
}

// Removes the observer associated with a property.
// Attempts an unobserve_property request, but fails silently.
func (c *Client) UnobserveProperty(id int) {
	c.observerMu.Lock()
	delete(c.observers, id)
	c.observerMu.Unlock()

	go c.Request("unobserve_property", id)
}

// Closes the IPC client. Subsequent requests will fail with ErrClosed.
func (c *Client) Close() {
	c.closed = true

	close(c.receiver)
	c.conn.Close()
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

		c.requestMu.Lock()
		if c.requests == nil {
			c.requests = make(map[int]*ipcRequest, 1)
		}
		
		c.requests[req.RequestID] = req
		c.requestMu.Unlock()

		// Realistically, this can never fail because the pipe has been closed,
		// since the read loop should immediately exit, and close the request channel.
		_, err = c.conn.Write(body)
		if err != nil {
			c.publishError(err)
			continue
		}
		_, err = c.conn.Write([]byte{'\n'})
		if err != nil {
			c.publishError(err)
			continue
		}
	}
}

func (c *Client) read() {
	reader := bufio.NewReader(c.conn)
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

func (c *Client) requestSend(request *ipcRequest) (any, error) {
	if c.closed {
		return nil, ErrClosed
	}

	c.receiver <- request
	response := <-request.responseChan
	if response.Error != "success" {
		return nil, &MpvError{response.Error}
	}

	return response.Data, nil	
}

func (c *Client) requestInternal(command []any, async bool) (any, error) {
	request := &ipcRequest{
		Command: command,
		RequestID: rand.Int(),
		Async: async,
		responseChan: make(chan *ipcResponse),
	}

	return c.requestSend(request)
}

func (c *Client) dispatch(response *ipcResponse) {
	switch response.Event {
	case "":
		c.requestMu.Lock()
		defer c.requestMu.Unlock()
	
		if c.requests == nil {
			return
		}
		
		request, ok := c.requests[response.RequestID]
		if !ok {
			return
		}

		request.responseChan <- response
		close(request.responseChan)
		delete(c.requests, response.RequestID)
	case "property-change":
		c.observerMu.Lock()
		defer c.observerMu.Unlock()
		
		if c.observers == nil {
			return
		}

		observer, ok := c.observers[response.ID]
		if ok {
			go observer(response.Data)
		}
	default:
		c.listenerMu.Lock()
		defer c.listenerMu.Unlock()

		if c.listeners == nil {
			return
		}

		listener, ok := c.listeners[response.Event]
		if ok {
			go listener(response.EventData)
		}
	}
}

func (c *Client) publishError(err error) {
	if c.onError != nil {
		c.onError(err)
	}
}
