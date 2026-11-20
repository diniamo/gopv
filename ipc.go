package gopv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"sync"

	"go.pennock.tech/swallowjson"
)

// IPC client data
type Client struct {
	conn        net.Conn

	requestChan chan request
	requests    map[int64]chan<- any
	requestID   int64

	listeners   map[string][]*subscription[map[string]any]

	observers   map[int64]*subscription[any]
	observerID  int64

	ctx         context.Context
	cancel      func()
	close       sync.Once
	mu          sync.Mutex
	closed      bool
}

// Error sent by mpv
type IPCError struct {
	Status string
}
func (e *IPCError) Error() string {
	return e.Status
}

var ErrClosed = errors.New("IPC client is closed")

// Send an IPC request.
// Errors are also sent through the channel.
func (c *Client) Request(command ...any) <-chan any {
	// IMPORTANT: this has to be buffered, so the dispatcher goroutine won't hang if the user ignores the result
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
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, ErrClosed
	}

	ctx, cancel := context.WithCancel(c.ctx)
	out := make(chan map[string]any)
	sub := &subscription[map[string]any]{
		ctx: ctx,
		queue: []map[string]any{},
		out: out,
		wake: make(chan struct{}, 1),
	}
	go sub.pump()

	listeners := c.listeners[event]
	if listeners == nil {
		c.listeners[event] = []*subscription[map[string]any]{sub}
	} else {
		c.listeners[event] = append(c.listeners[event], sub)
	}

	c.mu.Unlock()
	return out, func() {
		c.mu.Lock()

		listeners := c.listeners[event]
		for i, s := range listeners {
			if s == sub {
				c.listeners[event] = slices.Delete(listeners, i, i + 1)
				break
			}
		}
		cancel()

		c.mu.Unlock()
	}, nil
}

// Start observing a property.
// The returned function can be used to stop observing and close the channel.
func (c *Client) ObserveProperty(property string) (<-chan any, func(), error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, ErrClosed
	}

	id := c.observerID
	c.observerID += 1

	ctx, cancel := context.WithCancel(c.ctx)
	out := make(chan any)
	sub := &subscription[any]{
		ctx: ctx,
		queue: []any{},
		out: out,
		wake: make(chan struct{}, 1),
	}
	c.observers[id] = sub
	go sub.pump()

	c.mu.Unlock()
	response := <-c.Request("observe_property", id, property)
	if err, ok := response.(error); ok {
		close(out)
		cancel()
		delete(c.observers, id)
		return nil, nil, err
	}

	return out, func() {
		c.mu.Lock()
		cancel()
		delete(c.observers, id)
		c.mu.Unlock()

		c.Request("unobserve_property", id)
	}, nil
}

// Close the client.
func (c *Client) Close() {
	c.close.Do(func() {
		c.mu.Lock()
		c.closed = true

		c.cancel()
		c.conn.Close()

		for _, ch := range c.requests {
			ch <- ErrClosed
			close(ch)
		}
		clear(c.requests)
		clear(c.listeners)
		clear(c.observers)

		c.mu.Unlock()
	})
}

// SECTION: internal

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

type subscription[T any] struct {
	mu    sync.Mutex
	ctx   context.Context
	queue []T
	out   chan<- T
	wake  chan struct{}
}

func connectInternal(conn net.Conn) *Client {
	ctx, cancel := context.WithCancel(context.Background())

	client := &Client{
		conn:        conn,
		requestChan: make(chan request),
		requests:    make(map[int64]chan<- any),
		listeners:   make(map[string][]*subscription[map[string]any]),
		observers:   make(map[int64]*subscription[any]),
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
	id := c.requestID

	body, err := json.Marshal(ipcRequest{
		Command:   request.command,
		RequestID: id,
		Async:     true,
	})
	if err != nil {
		request.responseChan <- err
		close(request.responseChan)
		return
	}

	c.mu.Lock()
	c.requests[id] = request.responseChan
	c.requestID += 1
	c.mu.Unlock()

	_, err = c.conn.Write(append(body, '\n'))
	if err != nil {
		c.mu.Lock()
		if _, ok := c.requests[id]; ok {
			request.responseChan <- err
			close(request.responseChan)
			delete(c.requests, id)
		}
		c.mu.Unlock()
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

		c.dispatch(response)
	}
}

func (c *Client) dispatch(response ipcResponse) {
	c.mu.Lock()

	switch response.Event {
	case "":
		if responseChan, ok := c.requests[response.RequestID]; ok {
			if response.Error != "success" {
				responseChan <- error(&IPCError{response.Error})
			} else {
				responseChan <- response.Data
			}

			close(responseChan)
			delete(c.requests, response.RequestID)
		}

	case "property-change":
		if sub, ok := c.observers[response.ID]; ok {
			sub.push(response.Data)
		}

	default:
		if listeners, ok := c.listeners[response.Event]; ok {
			for _, sub := range listeners {
				sub.push(response.EventData)
			}
		}
	}

	c.mu.Unlock()
}

func (s *subscription[T]) pump() {
	outer: for {
		select {
		case <-s.ctx.Done():
			break outer
		case <-s.wake:
		}

		for {
			s.mu.Lock()
			if len(s.queue) > 0 {
				value := s.queue[0]

				var zero T
				s.queue[0] = zero
				s.queue = s.queue[1:]

				s.mu.Unlock()

				select {
				case <-s.ctx.Done():
					break outer
				case s.out <- value:
				}
			} else {
				s.mu.Unlock()
				break
			}
		}
	}

	close(s.out)
}

func (s *subscription[T]) push(v T) {
	s.mu.Lock()
	s.queue = append(s.queue, v)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.mu.Unlock()
}
