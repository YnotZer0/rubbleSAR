package cot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"rubblesar/internal/diag"
)

// ClientConfig configures a TAK server connection (plain TCP).
type ClientConfig struct {
	// Addr is host:port of the TAK server (taky: 8087).
	Addr string
	// Hello returns what to send first on every (re)connection: our own
	// position and the full picture, so a restarted server catches up.
	Hello func(now time.Time) []*Event
	// OnEvent receives every event from the server (called from the reader).
	OnEvent func(*Event)
	// OnState is told about connections and disconnections.
	OnState func(connected bool, err error)
	// PingUID is the uid of the keep-alive pings ("" = no pings).
	PingUID      string
	PingEvery    time.Duration // default 15 s
	DialTimeout  time.Duration // default 5 s
	WriteTimeout time.Duration // default 5 s
	MaxBackoff   time.Duration // default 30 s
	QueueLen     int           // events kept while disconnected (default 2000; oldest dropped)
	Log          *slog.Logger
}

// ClientStats describes the connection.
type ClientStats struct {
	Addr       string    `json:"addr"`
	Connected  bool      `json:"connected"`
	Since      time.Time `json:"since"`
	Sent       uint64    `json:"sent"`
	Received   uint64    `json:"received"`
	Dropped    uint64    `json:"dropped"`
	Skipped    uint64    `json:"skipped"`
	Reconnects uint64    `json:"reconnects"`
	Queued     int       `json:"queued"`
	LastError  string    `json:"last_error,omitempty"`
}

// Client keeps a connection to a TAK server open, reconnecting with backoff.
type Client struct {
	cfg  ClientConfig
	log  *slog.Logger
	wake chan struct{}

	mu    sync.Mutex
	queue []*Event
	st    ClientStats
	conn  net.Conn
	done  bool
}

// NewClient makes a client; Run connects it.
func NewClient(cfg ClientConfig) *Client {
	if cfg.PingEvery <= 0 {
		cfg.PingEvery = 15 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 5 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.QueueLen <= 0 {
		cfg.QueueLen = 2000
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Client{cfg: cfg, log: log, wake: make(chan struct{}, 1), st: ClientStats{Addr: cfg.Addr}}
}

// Send queues an event (false when the client is closed).
func (c *Client) Send(e *Event) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return false
	}
	if len(c.queue) >= c.cfg.QueueLen {
		c.queue = c.queue[1:]
		c.st.Dropped++
	}
	c.queue = append(c.queue, e)
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return true
}

// Stats returns a snapshot.
func (c *Client) Stats() ClientStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.st
	s.Queued = len(c.queue)
	return s
}

// Connected reports whether the connection is up.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.Connected
}

// Run connects and keeps the connection up until ctx ends.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	first := true
	for ctx.Err() == nil {
		d := net.Dialer{Timeout: c.cfg.DialTimeout, KeepAlive: 15 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", c.cfg.Addr)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			err = diag.Wrap("E-TAK-001", err, "TAK server %s", c.cfg.Addr)
			c.setState(false, err)
			if first {
				c.log.Warn("cannot reach the TAK server; retrying", "addr", c.cfg.Addr, "err", err)
			} else {
				c.log.Debug("TAK server still unreachable", "addr", c.cfg.Addr, "err", err)
			}
			first = false
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, c.cfg.MaxBackoff)
			continue
		}
		backoff = time.Second
		first = true
		err = c.serve(ctx, conn)
		if ctx.Err() != nil {
			break
		}
		c.log.Warn("TAK server connection lost; reconnecting", "addr", c.cfg.Addr, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	c.mu.Lock()
	c.done = true
	c.st.Connected = false
	c.mu.Unlock()
}

func (c *Client) setState(up bool, err error) {
	c.mu.Lock()
	c.st.Connected = up
	if up {
		c.st.Since = time.Now()
	}
	if err != nil {
		c.st.LastError = err.Error()
	}
	c.mu.Unlock()
	if c.cfg.OnState != nil {
		c.cfg.OnState(up, err)
	}
}

func (c *Client) serve(ctx context.Context, conn net.Conn) error {
	c.mu.Lock()
	c.conn = conn
	if !c.st.Since.IsZero() {
		c.st.Reconnects++
	}
	c.mu.Unlock()
	c.setState(true, nil)
	c.log.Info("connected to the TAK server", "addr", c.cfg.Addr)
	readErr := make(chan error, 1)
	go func() {
		sp := NewSplitter(conn)
		for {
			e, err := sp.ReadEvent()
			c.mu.Lock()
			c.st.Skipped = uint64(sp.Skipped)
			c.mu.Unlock()
			if err != nil {
				readErr <- err
				return
			}
			c.mu.Lock()
			c.st.Received++
			c.mu.Unlock()
			if c.cfg.OnEvent != nil {
				c.cfg.OnEvent(e)
			}
		}
	}()
	defer func() {
		conn.Close()
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
	}()
	write := func(e *Event) error {
		b, err := e.Marshal()
		if err != nil {
			c.log.Warn("skipping an event", "uid", e.UID, "err", err)
			return nil
		}
		_ = conn.SetWriteDeadline(time.Now().Add(c.cfg.WriteTimeout))
		if _, err := conn.Write(b); err != nil {
			return diag.Wrap("E-TAK-002", err, "writing to the TAK server %s", c.cfg.Addr)
		}
		c.mu.Lock()
		c.st.Sent++
		c.mu.Unlock()
		return nil
	}
	fail := func(err error) error {
		c.setState(false, err)
		return err
	}
	if c.cfg.Hello != nil {
		for _, e := range c.cfg.Hello(time.Now()) {
			if err := write(e); err != nil {
				return fail(err)
			}
		}
	}
	ping := time.NewTicker(c.cfg.PingEvery)
	defer ping.Stop()
	for {
		for {
			c.mu.Lock()
			if len(c.queue) == 0 {
				c.mu.Unlock()
				break
			}
			e := c.queue[0]
			c.queue = c.queue[1:]
			c.mu.Unlock()
			if err := write(e); err != nil {
				return fail(err)
			}
		}
		select {
		case <-ctx.Done():
			c.setState(false, nil)
			return ctx.Err()
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				err = diag.New("E-TAK-002", "the TAK server %s closed the connection", c.cfg.Addr)
			} else {
				err = diag.Wrap("E-TAK-002", err, "reading from the TAK server %s", c.cfg.Addr)
			}
			return fail(err)
		case <-c.wake:
		case <-ping.C:
			if c.cfg.PingUID != "" {
				if err := write(Ping(c.cfg.PingUID, time.Now())); err != nil {
					return fail(err)
				}
			}
		}
	}
}

// Close drops the connection (Run returns when its context ends).
func (c *Client) Close() {
	c.mu.Lock()
	c.done = true
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()
}
