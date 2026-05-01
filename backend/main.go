package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sort"
	"strings"
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
	WorkDir   string    `json:"workDir,omitempty"`
	EnvVars   map[string]string `json:"envVars,omitempty"`
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
	persistPath string
	persistMu   sync.Mutex
}

func NewConsoleManager() *ConsoleManager {
	return &ConsoleManager{
		consoles:    make(map[string]*Console),
		persistPath: runtimeConfigPath(".consoles.json"),
	}
}

type persistedConsole struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	WorkDir   string            `json:"workDir,omitempty"`
	EnvVars   map[string]string `json:"envVars,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	AutoStart bool              `json:"autoStart"`
}

func (cm *ConsoleManager) saveToDisk() error {
	cm.mu.RLock()
	records := make([]persistedConsole, 0, len(cm.consoles))
	for _, c := range cm.consoles {
		records = append(records, persistedConsole{
			ID:        c.ID,
			Name:      c.Name,
			Command:   c.Command,
			Args:      append([]string(nil), c.Args...),
			WorkDir:   c.WorkDir,
			EnvVars:   cloneEnvVars(c.EnvVars),
			CreatedAt: c.CreatedAt,
			AutoStart: c.AutoStart,
		})
	}
	cm.mu.RUnlock()

	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID < records[j].ID
		}
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})

	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal consoles: %w", err)
	}

	cm.persistMu.Lock()
	defer cm.persistMu.Unlock()

	tmpPath := cm.persistPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write temp consoles file: %w", err)
	}
	if err := os.Rename(tmpPath, cm.persistPath); err != nil {
		_ = os.Remove(cm.persistPath)
		if err2 := os.Rename(tmpPath, cm.persistPath); err2 != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("replace consoles file: %w", err2)
		}
	}
	return nil
}

func (cm *ConsoleManager) loadFromDisk() error {
	cm.persistMu.Lock()
	data, err := os.ReadFile(cm.persistPath)
	cm.persistMu.Unlock()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read consoles file: %w", err)
	}

	if len(data) == 0 {
		return nil
	}

	var records []persistedConsole
	if err := json.Unmarshal(data, &records); err != nil {
		return fmt.Errorf("decode consoles file: %w", err)
	}

	loaded := make(map[string]*Console, len(records))
	for _, record := range records {
		id := strings.TrimSpace(record.ID)
		command := strings.TrimSpace(record.Command)
		if id == "" || command == "" {
			continue
		}

		createdAt := record.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now()
		}

		name := strings.TrimSpace(record.Name)
		if name == "" {
			name = command
		}

		loaded[id] = &Console{
			ID:        id,
			Name:      name,
			Command:   command,
			Args:      append([]string(nil), record.Args...),
			WorkDir:   strings.TrimSpace(record.WorkDir),
			EnvVars:   cloneEnvVars(record.EnvVars),
			Status:    "stopped",
			CreatedAt: createdAt,
			AutoStart: record.AutoStart,
			output:    NewRingBuffer(256 * 1024),
		}
	}

	cm.mu.Lock()
	cm.consoles = loaded
	cm.mu.Unlock()
	return nil
}

func (cm *ConsoleManager) restoreAutoStartConsoles() {
	consoles := cm.List()
	sort.Slice(consoles, func(i, j int) bool {
		return consoles[i].CreatedAt.Before(consoles[j].CreatedAt)
	})

	for _, c := range consoles {
		if !c.AutoStart {
			continue
		}
		if err := cm.Start(c.ID); err != nil {
			log.Printf("AutoStart failed for %s (%s): %v", c.Name, c.ID, err)
		}
	}
}

func (cm *ConsoleManager) Create(name, command string, args []string, workDir string, envVars map[string]string, autoStart bool) (*Console, error) {
	if workDir != "" {
		fi, err := os.Stat(workDir)
		if err != nil {
			return nil, fmt.Errorf("workDir not found: %w", err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("workDir is not a directory: %s", workDir)
		}
	}

	c := &Console{
		ID:        uuid.New().String(),
		Name:      name,
		Command:   command,
		Args:      args,
		WorkDir:   workDir,
		EnvVars:   cloneEnvVars(envVars),
		Status:    "stopped",
		CreatedAt: time.Now(),
		AutoStart: autoStart,
		output:    NewRingBuffer(256 * 1024), // 256KB ring buffer
	}

	cm.mu.Lock()
	cm.consoles[c.ID] = c
	cm.mu.Unlock()

	if err := cm.saveToDisk(); err != nil {
		cm.mu.Lock()
		delete(cm.consoles, c.ID)
		cm.mu.Unlock()
		return nil, fmt.Errorf("failed to persist console: %w", err)
	}

	if autoStart {
		return c, cm.Start(c.ID)
	}
	return c, nil
}

func (cm *ConsoleManager) Update(id, name, command string, args []string, workDir string) (*Console, error) {
	command = strings.TrimSpace(command)
	name = strings.TrimSpace(name)
	workDir = strings.TrimSpace(workDir)
	if command == "" {
		return nil, fmt.Errorf("command is required")
	}
	if name == "" {
		name = command
	}
	if workDir != "" {
		fi, err := os.Stat(workDir)
		if err != nil {
			return nil, fmt.Errorf("workDir not found: %w", err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("workDir is not a directory: %s", workDir)
		}
	}

	c, ok := cm.Get(id)
	if !ok {
		return nil, fmt.Errorf("console not found: %s", id)
	}

	c.mu.Lock()
	prevName := c.Name
	prevCommand := c.Command
	prevArgs := append([]string(nil), c.Args...)
	prevWorkDir := c.WorkDir
	c.Name = name
	c.Command = command
	c.Args = append([]string(nil), args...)
	c.WorkDir = workDir
	c.mu.Unlock()

	if err := cm.saveToDisk(); err != nil {
		c.mu.Lock()
		c.Name = prevName
		c.Command = prevCommand
		c.Args = prevArgs
		c.WorkDir = prevWorkDir
		c.mu.Unlock()
		return nil, fmt.Errorf("failed to persist console update: %w", err)
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

func cloneEnvVars(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		k := strings.TrimSpace(key)
		if k == "" {
			continue
		}
		dst[k] = value
	}
	if len(dst) == 0 {
		return nil
	}
	return dst
}

func buildCommandEnv(overrides map[string]string) []string {
	envMap := make(map[string]string)
	for _, e := range os.Environ() {
		key, value, ok := strings.Cut(e, "=")
		if !ok || key == "" {
			continue
		}
		// Prevent manager PORT from leaking into instance process.
		if key == "PORT" {
			continue
		}
		envMap[key] = value
	}
	if _, ok := envMap["TERM"]; !ok || envMap["TERM"] == "" {
		envMap["TERM"] = "xterm-256color"
	}
	envMap["CONSOLE_MANAGER"] = "true"

	for key, value := range overrides {
		k := strings.TrimSpace(key)
		if k == "" {
			continue
		}
		envMap[k] = value
	}

	keys := make([]string, 0, len(envMap))
	for key := range envMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+envMap[key])
	}
	return out
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
	if c.WorkDir != "" {
		c.cmd.Dir = c.WorkDir
	}
	c.cmd.Env = buildCommandEnv(c.EnvVars)

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

func (cm *ConsoleManager) StopAll() {
	consoles := cm.List()
	sort.Slice(consoles, func(i, j int) bool {
		return consoles[i].CreatedAt.Before(consoles[j].CreatedAt)
	})

	for _, c := range consoles {
		c.mu.Lock()
		running := c.Status == "running"
		c.mu.Unlock()
		if !running {
			continue
		}

		log.Printf("Stopping instance %s (%s)", c.Name, c.ID)
		if err := cm.Stop(c.ID); err != nil {
			log.Printf("Failed to stop instance %s (%s): %v", c.Name, c.ID, err)
		}
	}
}

func (cm *ConsoleManager) Delete(id string) error {
	_ = cm.Stop(id)
	cm.mu.Lock()
	delete(cm.consoles, id)
	cm.mu.Unlock()
	if err := cm.saveToDisk(); err != nil {
		return fmt.Errorf("failed to persist console deletion: %w", err)
	}
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
	WorkDir   string   `json:"workDir"`
	EnvVars   map[string]string `json:"envVars"`
	AutoStart bool     `json:"autoStart"`
}

type UpdateRequest struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	WorkDir string   `json:"workDir"`
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

func runtimeConfigPath(filename string) string {
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join(".", filename)
	}
	return filepath.Join(filepath.Dir(exe), filename)
}

func getOrCreateCredentials() (username, passwordHash string) {
	configPath := runtimeConfigPath(".credentials")

	if data, err := os.ReadFile(configPath); err == nil {
		parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
		if len(parts) == 2 {
			return parts[0], parts[1]
		}
	}

	// Generate random password
	b := make([]byte, 12)
	rand.Read(b)
	password := hex.EncodeToString(b)[:12]
	username = "admin"
	hash := sha256.Sum256([]byte(password))
	passwordHash = hex.EncodeToString(hash[:])

	os.WriteFile(configPath, []byte(username+":"+passwordHash), 0600)
	log.Printf("========================================")
	log.Printf("  Generated credentials:")
	log.Printf("  Username: %s", username)
	log.Printf("  Password: %s", password)
	log.Printf("  (Saved to %s)", configPath)
	log.Printf("========================================")
	return username, passwordHash
}

func basicAuthMiddleware(expectedUser, expectedPassHash string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Login API endpoint
		if r.URL.Path == "/api/login" && r.Method == http.MethodPost {
			var creds struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
				jsonResp(w, 400, APIResponse{Error: "invalid request"})
				return
			}
			hash := sha256.Sum256([]byte(creds.Password))
			passHash := hex.EncodeToString(hash[:])
			if subtle.ConstantTimeCompare([]byte(creds.Username), []byte(expectedUser)) == 1 &&
				subtle.ConstantTimeCompare([]byte(passHash), []byte(expectedPassHash)) == 1 {
				// Set auth cookie
				http.SetCookie(w, &http.Cookie{
					Name:     "cm_auth",
					Value:    expectedPassHash[:16],
					Path:     "/",
					HttpOnly: true,
					MaxAge:   86400 * 7,
				})
				jsonResp(w, 200, APIResponse{Success: true})
			} else {
				jsonResp(w, 401, APIResponse{Error: "用户名或密码错误"})
			}
			return
		}

		// Check auth cookie
		cookie, err := r.Cookie("cm_auth")
		if err == nil && cookie.Value == expectedPassHash[:16] {
			next.ServeHTTP(w, r)
			return
		}

		// For API requests, return 401
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
			jsonResp(w, 401, APIResponse{Error: "unauthorized"})
			return
		}

		// For page requests, serve the page (login overlay handled by frontend)
		next.ServeHTTP(w, r)
	})
}

func getOrCreatePort() string {
	// Check environment variable first
	if p := os.Getenv("PORT"); p != "" {
		return p
	}

	// Determine config file path next to the executable
	configPath := runtimeConfigPath(".port")

	// Try to read saved port
	if data, err := os.ReadFile(configPath); err == nil {
		port := string(data)
		if _, err := strconv.Atoi(port); err == nil {
			return port
		}
	}

	// Pick a random available port
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Fatal("failed to find available port:", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()

	// Save for future runs
	os.WriteFile(configPath, []byte(port), 0644)
	return port
}

func main() {
	mgr := NewConsoleManager()
	if err := mgr.loadFromDisk(); err != nil {
		log.Printf("failed to load persisted consoles: %v", err)
	}
	mgr.restoreAutoStartConsoles()

	addr := ":" + getOrCreatePort()
	authUser, authPassHash := getOrCreateCredentials()

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
			req.WorkDir = strings.TrimSpace(req.WorkDir)
			if req.WorkDir != "" {
				if abs, err := filepath.Abs(req.WorkDir); err == nil {
					req.WorkDir = abs
				}
			}
			c, err := mgr.Create(req.Name, req.Command, req.Args, req.WorkDir, req.EnvVars, req.AutoStart)
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

		case action == "" && r.Method == http.MethodPut:
			var req UpdateRequest
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
			req.WorkDir = strings.TrimSpace(req.WorkDir)
			if req.WorkDir != "" {
				if abs, err := filepath.Abs(req.WorkDir); err == nil {
					req.WorkDir = abs
				}
			}
			c, err := mgr.Update(id, req.Name, req.Command, req.Args, req.WorkDir)
			if err != nil {
				jsonResp(w, 500, APIResponse{Error: err.Error()})
				return
			}
			jsonResp(w, 200, APIResponse{Success: true, Data: c})

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
	handler := basicAuthMiddleware(authUser, authPassHash, mux)
	server := &http.Server{Addr: addr, Handler: handler}

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		log.Fatal(err)
	case <-sigCtx.Done():
		log.Printf("Shutdown signal received, stopping all instances...")
	}

	mgr.StopAll()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}
	log.Printf("Console Manager stopped")
}
