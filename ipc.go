package gopv

import (
	"bufio"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"

	"go.pennock.tech/swallowjson"
)

// IPC client
type Client struct {
	conn        net.Conn

	requestChan chan request
	requestMu   sync.Mutex
	requests    map[int64]chan any
	requestID   int64

	listenerMu  sync.RWMutex
	listeners   map[string]*list.List

	observerMu  sync.RWMutex
	observers   map[int64]chan<- any
	observerID  int64

	close       sync.Once
	ctx         context.Context
	cancel      func()
}

// Error sent by mpv
type IPCError struct {
	status string
}
func (e IPCError) Error() string {
	return e.status
}

var ErrClosed = errors.New("IPC client is closed")

// Send an IPC request.
// Errors are also sent through the channel.
func (c *Client) Request(command ...any) <-chan any {
	// IMPORTANT: this has to be buffered, so we dispatcher goroutine won't hang, if the user ignores the result
	responseChan := make(chan any, 1)

	select {
	case c.requestChan <- request{command, responseChan}:
	case <-c.ctx.Done():
		responseChan <- ErrClosed
		close(responseChan)
	}

	return responseChan
}

// Subscribe to an event.
// The map data sent through the channel may be nil.
// The returned function can be used to unsubscribe and close the channel.
func (c *Client) Subscribe(event string) (<-chan map[string]any, func(), error) {
	if c.ctx.Err() != nil {
		return nil, nil, ErrClosed
	}

	c.listenerMu.Lock()

	var listeners *list.List
	if c.listeners == nil {
		c.listeners        = make(map[string]*list.List, 1)
		listeners          = list.New()
		c.listeners[event] = listeners
	} else {
		listeners = c.listeners[event]
		if listeners == nil {
			listeners          = list.New()
			c.listeners[event] = listeners
		}
	}

	listener := make(chan map[string]any)
	element  := listeners.PushBack(chan<- map[string]any(listener))

	c.listenerMu.Unlock()

	return listener, func() {
		c.listenerMu.Lock()

		c.listeners[event].Remove(element)
		close(listener)

		c.listenerMu.Unlock()
	}, nil
}

// Start observing a property.
// The returned function can be used to stop observing and close the channel.
func (c *Client) ObserveProperty(property string) (<-chan any, func(), error) {
	c.observerMu.Lock()

	if c.observers == nil {
		c.observers = make(map[int64]chan<- any, 1)
	}

	id := c.observerID
	observer := make(chan any)
	c.observers[id] = observer
	c.observerID += 1

	c.observerMu.Unlock()

	response := <-c.Request("observe_property", id, property)
	if err, ok := response.(error); ok {
		c.observerMu.Lock()
		delete(c.observers, id)
		c.observerMu.Unlock()

		return nil, nil, err
	}

	return observer, func() {
		c.observerMu.Lock()

		delete(c.observers, id)
		close(observer)

		c.observerMu.Unlock()
	}, nil
}

// Close the client.
func (c *Client) Close() {
	c.close.Do(func() {
		c.cancel()
		c.conn.Close()

		c.requestMu.Lock()
		for _, ch := range c.requests {
			close(ch)
		}
		clear(c.requests)
		c.requestMu.Unlock()

		c.listenerMu.Lock()
		for _, listeners := range c.listeners {
			for e := listeners.Front(); e != nil; e = e.Next() {
				close(e.Value.(chan<- map[string]any))
			}
		}
		clear(c.listeners)
		c.listenerMu.Unlock()

		c.observerMu.Lock()
		for _, ch := range c.observers {
			close(ch)
		}
		clear(c.observers)
		c.observerMu.Unlock()
	})
}


type request struct {
	command      []any
	responseChan chan any
}

type ipcRequest struct {
	Command   []any `json:"command"`
	RequestID int64 `json:"request_id"`
	Async     bool  `json:"async"`
}

type ipcResponse struct {
	Error     string         `json:"error"`
	Data      any            `json:"data"`
	RequestID int64          `json:"request_id"`
	Event     string         `json:"event"`
	EventData map[string]any `json:"-"`
	ID        int64          `json:"id"`
}

func connectInternal(conn net.Conn) *Client {
	ctx, cancel := context.WithCancel(context.Background())

	client := &Client{
		conn:        conn,
		requestChan: make(chan request),
		ctx:         ctx,
		cancel:      cancel,
	}

	go client.writeLoop()
	go client.readLoop()

	return client
}

func (c *Client) writeLoop() {
	for {
		select {
		case request := <-c.requestChan:
			c.sendRequest(request)
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Client) sendRequest(request request) {
	c.requestMu.Lock()

	id := c.requestID

	body, err := json.Marshal(ipcRequest{
		Command:   request.command,
		RequestID: id,
		Async:     true,
	})
	if err != nil {
		c.requestMu.Unlock()
		request.responseChan <- err
		close(request.responseChan)
		return
	}

	if c.requests == nil {
		c.requests = make(map[int64]chan any, 1)
	}
	c.requests[id] = request.responseChan
	c.requestID += 1

	c.requestMu.Unlock()

	_, err = c.conn.Write(append(body, '\n'))
	if err != nil {
		c.requestMu.Lock()
		delete(c.requests, id)
		c.requestMu.Unlock()

		request.responseChan <- err
		close(request.responseChan)

		return
	}
}

func (c *Client) readLoop() {
	reader := bufio.NewReader(c.conn)
	for {
		data, err := reader.ReadBytes('\n')
		if err != nil {
			c.Close()
			return
		}

		var response ipcResponse
		err = swallowjson.UnmarshalWith(&response, "EventData", data)
		if err != nil { continue }

		go c.dispatch(response)
	}
}

func (c *Client) dispatch(response ipcResponse) {
	switch response.Event {
	case "":
		c.requestMu.Lock()

		if c.requests == nil {
			return
		}

		if responseChan, ok := c.requests[response.RequestID]; ok {
			if response.Error != "success" {
				responseChan <- error(&IPCError{response.Error})
			} else {
				responseChan <- response.Data
			}

			close(responseChan)
			delete(c.requests, response.RequestID)
		}

		c.requestMu.Unlock()

	case "property-change":
		c.observerMu.RLock()

		if c.observers == nil {
			return
		}
		observer, ok := c.observers[response.ID]

		c.observerMu.RUnlock()

		if ok {
			observer <- response.Data
		}

	default:
		c.listenerMu.RLock()

		if c.listeners == nil {
			return
		}
		listeners, ok := c.listeners[response.Event]

		c.listenerMu.RUnlock()

		if ok {
			for element := listeners.Front(); element != nil; element = element.Next() {
				element.Value.(chan<- map[string]any) <- response.EventData
			}
		}
	}
}
