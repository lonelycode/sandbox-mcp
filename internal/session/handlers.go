package session

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/mark3labs/mcp-go/mcp"
)

const maxFileSize = 50 * 1024 * 1024 // 50MB

// NewSessionCreateHandler returns a handler for session_create.
func NewSessionCreateHandler(mgr *SessionManager) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sandboxID, ok := request.Params.Arguments["sandbox"].(string)
		if !ok || sandboxID == "" {
			return nil, fmt.Errorf("sandbox parameter is required")
		}

		sess, err := mgr.Create(ctx, sandboxID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to create session: %v", err)), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf(
			"Session created successfully.\n\nsession_id: %s\nsandbox: %s\nworking_directory: %s",
			sess.ID, sandboxID, sess.Config.Mount.WorkDir,
		)), nil
	}
}

// NewSessionExecHandler returns a handler for session_exec.
func NewSessionExecHandler(mgr *SessionManager) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, ok := request.Params.Arguments["session_id"].(string)
		if !ok || sessionID == "" {
			return nil, fmt.Errorf("session_id parameter is required")
		}

		command, ok := request.Params.Arguments["command"].(string)
		if !ok || command == "" {
			return nil, fmt.Errorf("command parameter is required")
		}

		timeoutSec := 30.0
		if t, ok := request.Params.Arguments["timeout_seconds"].(float64); ok && t > 0 {
			timeoutSec = t
			if timeoutSec > 300 {
				timeoutSec = 300
			}
		}

		sess, err := mgr.Get(sessionID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		sess.mu.Lock()
		defer sess.mu.Unlock()

		// Check that the container is still running
		inspect, err := sess.Client.ContainerInspect(ctx, sess.ContainerID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to inspect container: %v. The session may need to be recreated.", err)), nil
		}
		if inspect.State == nil || !inspect.State.Running {
			return mcp.NewToolResultError("Container is no longer running. Please destroy this session and create a new one."), nil
		}

		sess.touch()

		execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
		defer cancel()

		execConfig := container.ExecOptions{
			Cmd:          []string{"sh", "-c", command},
			AttachStdout: true,
			AttachStderr: true,
			WorkingDir:   sess.Config.Mount.WorkDir,
			User:         sess.Config.User,
		}

		execResp, err := sess.Client.ContainerExecCreate(execCtx, sess.ContainerID, execConfig)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to create exec: %v", err)), nil
		}

		response, err := sess.Client.ContainerExecAttach(execCtx, execResp.ID, container.ExecStartOptions{})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to attach to exec: %v", err)), nil
		}
		defer response.Close()

		stdout := new(bytes.Buffer)
		stderr := new(bytes.Buffer)
		if _, err := stdcopy.StdCopy(stdout, stderr, response.Reader); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to read exec output: %v", err)), nil
		}

		// Wait for exec to complete and get exit code
		for {
			inspectResp, err := sess.Client.ContainerExecInspect(execCtx, execResp.ID)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Failed to inspect exec: %v", err)), nil
			}
			if !inspectResp.Running {
				if inspectResp.ExitCode != 0 {
					output := stderr.String()
					if stdout.Len() > 0 {
						output = stdout.String() + "\nStderr:\n" + stderr.String()
					}
					if output == "" {
						output = fmt.Sprintf("Command failed with exit code %d", inspectResp.ExitCode)
					}
					return mcp.NewToolResultError(output), nil
				}

				if stderr.Len() > 0 {
					stdout.WriteString("\nStderr:\n")
					stdout.Write(stderr.Bytes())
				}

				return mcp.NewToolResultText(stdout.String()), nil
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// NewSessionWriteFileHandler returns a handler for session_write_file.
func NewSessionWriteFileHandler(mgr *SessionManager) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, ok := request.Params.Arguments["session_id"].(string)
		if !ok || sessionID == "" {
			return nil, fmt.Errorf("session_id parameter is required")
		}

		path, ok := request.Params.Arguments["path"].(string)
		if !ok || path == "" {
			return nil, fmt.Errorf("path parameter is required")
		}

		content, ok := request.Params.Arguments["content"].(string)
		if !ok {
			return nil, fmt.Errorf("content parameter is required")
		}

		sess, err := mgr.Get(sessionID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		sess.mu.Lock()
		defer sess.mu.Unlock()

		if sess.Config.Mount.ReadOnly {
			return mcp.NewToolResultError("This sandbox has a read-only mount. File writes are not supported."), nil
		}

		cleanPath, err := sanitizePath(path)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		fullPath := filepath.Join(sess.TempDir, cleanPath)

		// Create parent directories if needed
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to create directories: %v", err)), nil
		}

		if err := os.WriteFile(fullPath, []byte(content), sess.Config.Mount.ScriptPerms()); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to write file: %v", err)), nil
		}

		sess.touch()

		return mcp.NewToolResultText(fmt.Sprintf("File written: %s (%d bytes)", cleanPath, len(content))), nil
	}
}

// NewSessionReadFileHandler returns a handler for session_read_file.
func NewSessionReadFileHandler(mgr *SessionManager) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, ok := request.Params.Arguments["session_id"].(string)
		if !ok || sessionID == "" {
			return nil, fmt.Errorf("session_id parameter is required")
		}

		path, ok := request.Params.Arguments["path"].(string)
		if !ok || path == "" {
			return nil, fmt.Errorf("path parameter is required")
		}

		hostPath, _ := request.Params.Arguments["host_path"].(string)

		sess, err := mgr.Get(sessionID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		sess.mu.Lock()
		defer sess.mu.Unlock()

		cleanPath, err := sanitizePath(path)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		sess.touch()

		// Try reading from the host temp dir first (covers files in the bind-mounted workdir)
		fullPath := filepath.Join(sess.TempDir, cleanPath)
		data, err := readFileFromHost(fullPath)

		if err != nil {
			// Fall back to Docker copy API for files outside the mount
			containerPath := filepath.Join(sess.Config.Mount.WorkDir, cleanPath)
			data, err = readFileFromContainer(ctx, sess, containerPath)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Failed to read file: %v", err)), nil
			}
		}

		// Optionally copy to host path
		if hostPath != "" {
			dir := filepath.Dir(hostPath)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Failed to create host directory: %v", err)), nil
			}
			if err := os.WriteFile(hostPath, data, 0644); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Failed to write to host path: %v", err)), nil
			}
		}

		return mcp.NewToolResultText(string(data)), nil
	}
}

// NewSessionDestroyHandler returns a handler for session_destroy.
func NewSessionDestroyHandler(mgr *SessionManager) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, ok := request.Params.Arguments["session_id"].(string)
		if !ok || sessionID == "" {
			return nil, fmt.Errorf("session_id parameter is required")
		}

		if err := mgr.Destroy(sessionID); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		return mcp.NewToolResultText("Session destroyed successfully."), nil
	}
}

// readFileFromHost reads a file from the host filesystem with a size limit.
func readFileFromHost(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxFileSize {
		return nil, fmt.Errorf("file size %d bytes exceeds limit of %d bytes", info.Size(), maxFileSize)
	}
	return os.ReadFile(path)
}

// readFileFromContainer reads a file from the container via Docker's CopyFromContainer API.
func readFileFromContainer(ctx context.Context, sess *Session, containerPath string) ([]byte, error) {
	reader, _, err := sess.Client.CopyFromContainer(ctx, sess.ContainerID, containerPath)
	if err != nil {
		return nil, fmt.Errorf("docker copy failed: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, reader)
		reader.Close()
	}()

	return extractSingleFileFromTar(reader, maxFileSize)
}
