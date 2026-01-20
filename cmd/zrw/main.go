package main

import (
	"encoding/gob"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

// Timeouts
const (
	acceptTimeout         = 10 * time.Second // Wait for child to connect
	pidReceiveTimeout     = 5 * time.Second  // Wait for child to send PID
	livenessCheckInterval = 1 * time.Second  // Poll interval for child liveness
)

// Exit codes
const (
	exitSuccess = 0
	exitError   = 1
)

// Protocol types - simple, no union wrapper

type CommandRequest struct {
	Env  map[string]string
	Cmd  string
	Args []string
	Cwd  string
}

type CommandResult struct {
	ExitCode int
	Error    string // Empty if no error
}

// Protocol wraps a connection with gob encoder/decoder.
// Reusing encoder/decoder is important because gob may buffer internally.
type Protocol struct {
	conn net.Conn
	enc  *gob.Encoder
	dec  *gob.Decoder
}

func NewProtocol(conn net.Conn) *Protocol {
	return &Protocol{
		conn: conn,
		enc:  gob.NewEncoder(conn),
		dec:  gob.NewDecoder(conn),
	}
}

func (p *Protocol) Send(v any) error {
	return p.enc.Encode(v)
}

func (p *Protocol) Recv(v any) error {
	return p.dec.Decode(v)
}

func (p *Protocol) RecvWithTimeout(v any, timeout time.Duration) error {
	_ = p.conn.SetReadDeadline(time.Now().Add(timeout)) //nolint:errcheck
	defer p.conn.SetReadDeadline(time.Time{})           //nolint:errcheck
	return p.dec.Decode(v)
}

// Helper functions

func isProcessAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func acceptWithTimeout(listener *net.UnixListener, timeout time.Duration) (net.Conn, error) {
	_ = listener.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck
	return listener.Accept()
}

func getEnvMap() map[string]string {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		if idx := strings.Index(e, "="); idx > 0 {
			env[e[:idx]] = e[idx+1:]
		}
	}
	return env
}

// Parent mode

func runParent(zellijArgs []string, cmd string, cmdArgs []string) int {
	// Create temp directory with Unix socket
	tmpDir, err := os.MkdirTemp("", "zrw-*")
	if err != nil {
		log.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck

	socketPath := filepath.Join(tmpDir, "zrw.sock")

	// Start listening on socket
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		log.Fatalf("listen on socket: %v", err)
	}
	defer listener.Close() //nolint:errcheck

	// Get current working directory
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("get working directory: %v", err)
	}

	// Get path to ourselves
	selfPath, err := os.Executable()
	if err != nil {
		log.Fatalf("get executable path: %v", err)
	}

	// Build zellij command
	zellijCmdArgs := []string{"run"}
	zellijCmdArgs = append(zellijCmdArgs, zellijArgs...)
	zellijCmdArgs = append(zellijCmdArgs, "--")
	zellijCmdArgs = append(zellijCmdArgs, selfPath, fmt.Sprintf("--zrw-socket=%s", socketPath))

	// Execute zellij
	zellijCmd := exec.Command("zellij", zellijCmdArgs...)
	zellijCmd.Stdout = os.Stdout
	zellijCmd.Stderr = os.Stderr
	if err := zellijCmd.Run(); err != nil {
		log.Fatalf("run zellij: %v", err)
	}

	// Accept connection from child (with timeout)
	conn, err := acceptWithTimeout(listener, acceptTimeout)
	if err != nil {
		log.Fatalf("accept connection: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	proto := NewProtocol(conn)

	// Receive child PID
	var childPid int
	if err := proto.RecvWithTimeout(&childPid, pidReceiveTimeout); err != nil {
		log.Fatalf("read child pid: %v", err)
	}

	// Set up signal forwarding to child
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	go func() {
		for range sigCh {
			if childPid > 0 {
				_ = syscall.Kill(childPid, syscall.SIGINT) //nolint:errcheck
			}
		}
	}()

	// Send CommandRequest
	req := CommandRequest{
		Env:  getEnvMap(),
		Cmd:  cmd,
		Args: cmdArgs,
		Cwd:  cwd,
	}
	if err := proto.Send(&req); err != nil {
		log.Fatalf("send command request: %v", err)
	}

	// Wait for CommandResult (with periodic liveness check)
	for {
		var result CommandResult
		if err := proto.RecvWithTimeout(&result, livenessCheckInterval); err != nil {
			// Timeout - check if child is still alive
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				if !isProcessAlive(childPid) {
					log.Fatal("child process died unexpectedly")
				}
				continue
			}
			log.Fatalf("read command result: %v", err)
		}

		if result.Error != "" {
			fmt.Fprintf(os.Stderr, "zrw: command error: %s\n", result.Error)
		}

		return result.ExitCode
	}
}

// Child mode

func runChild(socketPath string) int {
	// Connect to socket
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		log.Fatalf("connect to socket: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	proto := NewProtocol(conn)

	// Send child PID
	pid := os.Getpid()
	if err := proto.Send(&pid); err != nil {
		log.Fatalf("send child pid: %v", err)
	}

	// Receive CommandRequest
	var req CommandRequest
	if err := proto.Recv(&req); err != nil {
		log.Fatalf("read command request: %v", err)
	}

	// Build environment
	var envList []string
	for k, v := range req.Env {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	// Execute command
	cmd := exec.Command(req.Cmd, req.Args...)
	cmd.Env = envList
	cmd.Dir = req.Cwd
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Start command
	if err := cmd.Start(); err != nil {
		// Send error result
		result := CommandResult{
			ExitCode: exitError,
			Error:    err.Error(),
		}
		if sendErr := proto.Send(&result); sendErr != nil {
			log.Printf("failed to send error result: %v", sendErr)
		}
		return exitError
	}

	// Ignore SIGINT after spawn (so Ctrl-C only kills command)
	signal.Ignore(syscall.SIGINT)

	// Wait for command to complete
	exitCode := exitSuccess
	var cmdError string

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = exitError
			cmdError = err.Error()
		}
	}

	// Send CommandResult
	result := CommandResult{
		ExitCode: exitCode,
		Error:    cmdError,
	}
	if err := proto.Send(&result); err != nil {
		log.Fatalf("send command result: %v", err)
	}

	return exitSuccess
}

// CLI parsing

func parseArgs(args []string) (zellijArgs []string, cmd string, cmdArgs []string, socketPath string) {
	// Check for version flag
	for _, arg := range args {
		if arg == "--version" || arg == "-v" {
			fmt.Println(version)
			os.Exit(exitSuccess)
		}
	}

	// Check for child mode (--zrw-socket=...)
	for _, arg := range args {
		if strings.HasPrefix(arg, "--zrw-socket=") {
			socketPath = strings.TrimPrefix(arg, "--zrw-socket=")
			return
		}
	}

	// Parent mode: find -- separator
	separatorIdx := -1
	for i, arg := range args {
		if arg == "--" {
			separatorIdx = i
			break
		}
	}

	if separatorIdx == -1 || separatorIdx == len(args)-1 {
		fmt.Fprintln(os.Stderr, "Usage: zrw [ZELLIJ_RUN_OPTIONS] -- <COMMAND> [ARGS...]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Examples:")
		fmt.Fprintln(os.Stderr, "  zrw -- ls -la")
		fmt.Fprintln(os.Stderr, "  zrw -f -- htop                    # floating pane")
		fmt.Fprintln(os.Stderr, "  zrw -f --width 80% -- vim file    # floating with size")
		fmt.Fprintln(os.Stderr, "  zrw -n \"build\" -- cargo build     # named pane")
		os.Exit(exitError)
	}

	zellijArgs = args[:separatorIdx]
	cmd = args[separatorIdx+1]
	if separatorIdx+2 < len(args) {
		cmdArgs = args[separatorIdx+2:]
	}

	return
}

func main() {
	log.SetPrefix("zrw: ")
	log.SetFlags(0)

	zellijArgs, cmd, cmdArgs, socketPath := parseArgs(os.Args[1:])

	if socketPath != "" {
		// Child mode
		os.Exit(runChild(socketPath))
	} else {
		// Parent mode
		os.Exit(runParent(zellijArgs, cmd, cmdArgs))
	}
}
