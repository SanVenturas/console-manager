package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

//go:embed static
var staticFiles embed.FS

// Console represents a managed console instance
type Console struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Command   string    `json:"command"`
	Args      []string  `json:"args"`
	Status    string    `json:"status"` // "running", "stopped", "exited"
	CreatedAt time.Time `json:"createdAt"`
	ExitCode  int       `json:"exitCode"`
	AutoStart bool      `json:"autoStart"`

	cmd       *exec.Cmd
	ptmx      *os.File
	mu        sync.Mutex
	output    *RingBuffer
	listeners map[chan []byte]struct{}
	lmu       sync.Mutex
}

// RingBuffer stores the last N bytes of output for replay
type RingBuffer struct {
	buf  []byte
	size int
	pos  int
	full bool
	mu   sync.Mutex
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{buf: make([]byte, size), size: size}
}

func (rb *RingBuffer) Write(p []byte) (int, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for _, b := range p {
		rb.buf[rb.pos] = b
		rb.pos = (rb.pos + 1) % rb.size
		if rb.pos == 0 {
			rb.full = true
		}
	}
	return len(p), nil
}

func (rb *RingBuffer) Bytes() []byte {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if !rb.full {
		return rb.buf[:rb.pos]
	}
	out := make([]byte, rb.size)
	copy(out, rb.buf[rb.pos:])
	copy(out[rb.size-rb.pos:], rb.buf[:rb.pos])
	return out
}

func (c *Console) Subscribe() chan []byte {
	ch := make(chan []byte, 64)
	c.lmu.Lock()
	if c.listeners == nil {
		c.listeners = make(map[chan []byte]struct{})
	}
	c.listeners[ch] = struct{}{}
	c.lmu.Unlock()
	return ch
}

func (c *Console) Unsubscribe(ch chan []byte) {
	c.lmu.Lock()
	delete(c.listeners, ch)
	c.lmu.Unlock()
	close(ch)
}

func (c *Console) broadcast(data []byte) {
	c.lmu.Lock()
	defer c.lmu.Unlock()
	for ch := range c.listeners {
		select {
		case ch <- data:
		default: // drop if slow consumer
		}
	}
}

// ConsoleManager manages all console instances
type ConsoleManager struct {
	consoles map[string]*Console
	mu       sync.RWMutex
}

func NewConsoleManager() *ConsoleManager {
	return &ConsoleManager{consoles: make(map[string]*Console)}
}

func (cm *ConsoleManager) Create(name, command string, args []string, autoStart bool) (*Console, error) {
	c := &Console{
		ID:        uuid.New().String(),
		Name:      name,
		Command:   command,
		Args:      args,
		Status:    "stopped",
		CreatedAt: time.Now(),
		AutoStart: autoStart,
		output:    NewRingBuffer(256 * 1024), // 256KB ring buffer
	}

	cm.mu.Lock()
	cm.consoles[c.ID] = c
	cm.mu.Unlock()

	if autoStart {
		return c, cm.Start(c.ID)
	}
	return c, nil
}

func (cm *ConsoleManager) Get(id string) (*Console, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	c, ok := cm.consoles[id]
	return c, ok
}

func (cm *ConsoleManager) List() []*Console {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	list := make([]*Console, 0, len(cm.consoles))
	for _, c := range cm.consoles {
		list = append(list, c)
	}
	return list
}

func (cm *ConsoleManager) Start(id string) error {
	c, ok := cm.Get(id)
	if !ok {
		return fmt.Errorf("console not found: %s", id)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.Status == "running" {
		return fmt.Errorf("console already running")
	}

	c.cmd = exec.Command(c.Command, c.Args...)
	c.cmd.Env = os.Environ()

	ptmx, err := pty.Start(c.cmd)
	if err != nil {
		return fmt.Errorf("failed to start pty: %w", err)
	}
	c.ptmx = ptmx
	c.Status = "running"
	c.ExitCode = 0

	// Background goroutine: read PTY output, store in ring buffer and broadcast
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				c.output.Write(data)
				c.broadcast(data)
			}
			if err != nil {
				break
			}
		}
		c.mu.Lock()
		if c.cmd.ProcessState != nil {
			c.ExitCode = c.cmd.ProcessState.ExitCode()
		}
		c.Status = "exited"
		c.mu.Unlock()
		// Notify all subscribers that stream ended
		c.broadcast(nil)
	}()

	// Wait for process to finish in background
	go func() {
		_ = c.cmd.Wait()
	}()

	return nil
}

func (cm *ConsoleManager) Stop(id string) error {
	c, ok := cm.Get(id)
	if !ok {
		return fmt.Errorf("console not found: %s", id)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.Status != "running" {
		return fmt.Errorf("console not running")
	}

	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
		go func() {
			time.Sleep(5 * time.Second)
			c.mu.Lock()
			if c.Status == "stopped" && c.cmd != nil && c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			c.mu.Unlock()
		}()
	}
	c.Status = "stopped"
	if c.ptmx != nil {
		c.ptmx.Close()
		c.ptmx = nil
	}
	return nil
}

func (cm *ConsoleManager) Delete(id string) error {
	_ = cm.Stop(id)
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.consoles, id)
	return nil
}

func (cm *ConsoleManager) Resize(id string, rows, cols uint16) error {
	c, ok := cm.Get(id)
	if !ok {
		return fmt.Errorf("console not found: %s", id)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ptmx == nil {
		return fmt.Errorf("no pty")
	}

	ws := struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	}{Row: rows, Col: cols}

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		c.ptmx.Fd(),
		syscall.TIOCSWINSZ,
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// HTTP Handlers
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type CreateRequest struct {
	Name      string   `json:"name"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	AutoStart bool     `json:"autoStart"`
}

type ResizeMsg struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

func jsonResp(w http.ResponseWriter, status int, resp APIResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

func main() {
	mgr := NewConsoleManager()
	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}

	mux := http.NewServeMux()

	// API: List consoles
	mux.HandleFunc("/api/consoles", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			jsonResp(w, 200, APIResponse{Success: true, Data: mgr.List()})
		case http.MethodPost:
			var req CreateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				jsonResp(w, 400, APIResponse{Error: err.Error()})
				return
			}
			if req.Command == "" {
				jsonResp(w, 400, APIResponse{Error: "command is required"})
				return
			}
			if req.Name == "" {
				req.Name = req.Command
			}
			c, err := mgr.Create(req.Name, req.Command, req.Args, req.AutoStart)
			if err != nil {
				jsonResp(w, 500, APIResponse{Error: err.Error()})
				return
			}
			jsonResp(w, 201, APIResponse{Success: true, Data: c})
		default:
			w.WriteHeader(405)
		}
	})

	// API: Single console operations
	mux.HandleFunc("/api/consoles/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/api/consoles/"):]

		// Handle action endpoints like /api/consoles/{id}/start
		action := ""
		for i, ch := range id {
			if ch == '/' {
				action = id[i+1:]
				id = id[:i]
				break
			}
		}

		switch {
		case action == "" && r.Method == http.MethodGet:
			c, ok := mgr.Get(id)
			if !ok {
				jsonResp(w, 404, APIResponse{Error: "not found"})
				return
			}
			jsonResp(w, 200, APIResponse{Success: true, Data: c})

		case action == "" && r.Method == http.MethodDelete:
			if err := mgr.Delete(id); err != nil {
				jsonResp(w, 500, APIResponse{Error: err.Error()})
				return
			}
			jsonResp(w, 200, APIResponse{Success: true})

		case action == "start" && r.Method == http.MethodPost:
			if err := mgr.Start(id); err != nil {
				jsonResp(w, 500, APIResponse{Error: err.Error()})
				return
			}
			jsonResp(w, 200, APIResponse{Success: true})

		case action == "stop" && r.Method == http.MethodPost:
			if err := mgr.Stop(id); err != nil {
				jsonResp(w, 500, APIResponse{Error: err.Error()})
				return
			}
			jsonResp(w, 200, APIResponse{Success: true})

		default:
			w.WriteHeader(405)
		}
	})

	// WebSocket: Terminal I/O
	mux.HandleFunc("/ws/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/ws/"):]
		c, ok := mgr.Get(id)
		if !ok {
			http.Error(w, "console not found", 404)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("ws upgrade error: %v", err)
			return
		}
		defer conn.Close()

		c.mu.Lock()
		running := c.ptmx != nil
		c.mu.Unlock()

		if !running {
			conn.WriteMessage(websocket.TextMessage, []byte("\r\n[Console not running]\r\n"))
			return
		}

		// Send buffered output
		history := c.output.Bytes()
		if len(history) > 0 {
			conn.WriteMessage(websocket.BinaryMessage, history)
		}

		// Subscribe to live output
		sub := c.Subscribe()
		defer c.Unsubscribe(sub)

		// Broadcast -> WebSocket
		done := make(chan struct{})
		go func() {
			defer close(done)
			for data := range sub {
				if data == nil {
					conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("\r\n[Process exited, code: %d]\r\n", c.ExitCode)))
					return
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
					return
				}
			}
		}()

		// WebSocket -> PTY (input)
		go func() {
			for {
				msgType, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if msgType == websocket.TextMessage {
					var resize ResizeMsg
					if json.Unmarshal(msg, &resize) == nil && resize.Type == "resize" {
						mgr.Resize(id, resize.Rows, resize.Cols)
						continue
					}
				}
				c.mu.Lock()
				p := c.ptmx
				c.mu.Unlock()
				if p != nil {
					_, _ = p.Write(msg)
				}
			}
		}()

		<-done
	})

	// Static files (embedded frontend)
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	log.Printf("Console Manager starting on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
