package cot

import (
	"net"
	"sort"
	"sync"
	"time"
)

// Server is a minimal TAK router over plain TCP, for tests and in-process
// runs (the field uses taky). Like taky it passes every event from one client
// to every other client, or only to the callsigns named in <marti><dest>;
// a client is known by the callsign of its own position reports.
type Server struct {
	ln net.Listener
	wg sync.WaitGroup

	mu      sync.Mutex
	clients map[*serverConn]bool
	events  uint64
}

type serverConn struct {
	c        net.Conn
	out      chan []byte
	callsign string
	uid      string
}

// ListenServer starts a router on addr ("127.0.0.1:0" for any free port).
func ListenServer(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, clients: map[*serverConn]bool{}}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Addr is the listening address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		sc := &serverConn{c: c, out: make(chan []byte, 4096)}
		s.mu.Lock()
		s.clients[sc] = true
		s.mu.Unlock()
		s.wg.Add(2)
		go s.writer(sc)
		go s.reader(sc)
	}
}

func (s *Server) writer(sc *serverConn) {
	defer s.wg.Done()
	for b := range sc.out {
		_ = sc.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := sc.c.Write(b); err != nil {
			sc.c.Close()
			for range sc.out {
			}
			return
		}
	}
}

func (s *Server) reader(sc *serverConn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		if s.clients[sc] {
			delete(s.clients, sc)
			close(sc.out)
		}
		s.mu.Unlock()
		sc.c.Close()
	}()
	sp := NewSplitter(sc.c)
	for {
		doc, err := sp.Next()
		if err != nil {
			return
		}
		e, err := Parse(doc)
		if err != nil {
			continue
		}
		if e.Type == "t-x-c-t" {
			continue
		}
		raw := append([]byte(nil), doc...)
		s.mu.Lock()
		s.events++
		if e.IsPLI() && e.Detail.Contact != nil {
			sc.callsign, sc.uid = e.Detail.Contact.Callsign, e.UID
		}
		dests := e.Dests()
		for o := range s.clients {
			if o == sc {
				continue
			}
			if len(dests) > 0 && !contains(dests, o.callsign) {
				continue
			}
			select {
			case o.out <- raw:
			default: // a client that does not read loses events, as with any TAK server
			}
		}
		s.mu.Unlock()
	}
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// Callsigns lists the connected clients that have reported a position.
func (s *Server) Callsigns() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for c := range s.clients {
		if c.callsign != "" {
			out = append(out, c.callsign)
		}
	}
	sort.Strings(out)
	return out
}

// Events counts the events routed.
func (s *Server) Events() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

// DropClients closes every connection (clients must reconnect).
func (s *Server) DropClients() {
	s.mu.Lock()
	for c := range s.clients {
		c.c.Close()
	}
	s.mu.Unlock()
}

// Close stops the router.
func (s *Server) Close() {
	s.ln.Close()
	s.DropClients()
	s.wg.Wait()
}
