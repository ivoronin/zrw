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

	"github.com/ivoronin/argsieve"
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

// ErrorMode specifies behavior when command exits with non-zero code
type ErrorMode int

const (
	ErrorModeNone ErrorMode = iota // Exit immediately
	ErrorModeKeep                  // Keep pane open, parent exits immediately
	ErrorModeWait                  // Keep pane open, parent waits for Ctrl-C
)

func (e *ErrorMode) UnmarshalText(text []byte) error {
	switch string(text) {
	case "exit":
		*e = ErrorModeNone
	case "keep":
		*e = ErrorModeKeep
	case "wait":
		*e = ErrorModeWait
	default:
		return fmt.Errorf("invalid error mode: %s (expected: exit, keep, wait)", text)
	}
	return nil
}

type CommandRequest struct {
	Env       map[string]string
	Cmd       string
	Args      []string
	ErrorMode ErrorMode
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

func runParent(zellijArgs []string, cmd string, cmdArgs []string, errorMode ErrorMode) int {
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
		Env:       getEnvMap(),
		Cmd:       cmd,
		Args:      cmdArgs,
		ErrorMode: errorMode,
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
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Set up signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)

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

	// Wait for command in goroutine
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- cmd.Wait()
	}()

	// Forward signals to command until it exits
	var waitErr error
loop:
	for {
		select {
		case waitErr = <-doneCh:
			break loop
		case sig := <-sigCh:
			_ = cmd.Process.Signal(sig) //nolint:errcheck
		}
	}

	// Process exit code
	exitCode := exitSuccess
	var cmdError string
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = exitError
			cmdError = waitErr.Error()
		}
	}

	// Wait on error: wait for Ctrl-C BEFORE sending result (blocks parent)
	if req.ErrorMode == ErrorModeWait && exitCode != 0 {
		fmt.Fprintf(os.Stderr, "[zrw] Command exited with code %d. Press Ctrl-C to continue.\n", exitCode)
		<-sigCh
	}

	// Send CommandResult
	result := CommandResult{
		ExitCode: exitCode,
		Error:    cmdError,
	}
	if err := proto.Send(&result); err != nil {
		log.Fatalf("send command result: %v", err)
	}

	// Keep on error: wait for Ctrl-C AFTER sending result (parent exits immediately)
	if req.ErrorMode == ErrorModeKeep && exitCode != 0 {
		fmt.Fprintf(os.Stderr, "[zrw] Command exited with code %d. Press Ctrl-C to close.\n", exitCode)
		<-sigCh
	}

	return exitSuccess
}

// CLI parsing

type zrwOptions struct {
	Version   bool      `short:"v" long:"version"`
	OnError   ErrorMode `long:"on-error"`
	ZrwSocket string    `long:"zrw-socket"`
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage: zrw [OPTIONS] [ZELLIJ_RUN_OPTIONS] -- <COMMAND> [ARGS...]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Options:")
	fmt.Fprintln(os.Stderr, "  --on-error <mode>  Behavior on command failure:")
	fmt.Fprintln(os.Stderr, "                       exit - zrw exits with command exit code (default)")
	fmt.Fprintln(os.Stderr, "                       keep - zrw exits with command exit code, pane waits for Ctrl-C")
	fmt.Fprintln(os.Stderr, "                       wait - pane waits for Ctrl-C, then zrw exits with command exit code")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  zrw -- ls -la")
	fmt.Fprintln(os.Stderr, "  zrw -f -- htop                    # floating pane")
	fmt.Fprintln(os.Stderr, "  zrw -f --width 80% -- vim file    # floating with size")
	fmt.Fprintln(os.Stderr, "  zrw -n \"build\" -- cargo build     # named pane")
	fmt.Fprintln(os.Stderr, "  zrw --on-error keep -- make       # keep pane on failure")
	fmt.Fprintln(os.Stderr, "  zrw --on-error=wait -- make       # wait for Ctrl-C")
	os.Exit(exitError)
}

// Zellij run options that take arguments (need to pass through with their values)
var zellijPassthrough = []string{
	"--cwd", "-d", "--direction", "--height", "-n", "--name",
	"--pinned", "--width", "-x", "-y",
}

func parseArgs(args []string) (zellijArgs []string, cmd string, cmdArgs []string, socketPath string, errorMode ErrorMode) {
	var opts zrwOptions
	cfg := &argsieve.Config{RequirePositionalDelimiter: true}
	remaining, positional, err := argsieve.Sift(&opts, args, zellijPassthrough, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zrw: %v\n", err)
		os.Exit(exitError)
	}

	if opts.Version {
		fmt.Println(version)
		os.Exit(exitSuccess)
	}

	if opts.ZrwSocket != "" {
		socketPath = opts.ZrwSocket
		return
	}

	if len(positional) == 0 {
		printUsage()
	}

	zellijArgs = remaining
	cmd = positional[0]
	cmdArgs = positional[1:]
	errorMode = opts.OnError

	return
}

func main() {
	log.SetPrefix("zrw: ")
	log.SetFlags(0)

	zellijArgs, cmd, cmdArgs, socketPath, errorMode := parseArgs(os.Args[1:])

	if socketPath != "" {
		// Child mode
		os.Exit(runChild(socketPath))
	} else {
		// Parent mode
		os.Exit(runParent(zellijArgs, cmd, cmdArgs, errorMode))
	}
}
