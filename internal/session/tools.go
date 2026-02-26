package session

import (
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// NewSessionCreateTool creates the session_create MCP tool definition.
func NewSessionCreateTool(sandboxIDs []string) mcp.Tool {
	desc := "Create a persistent sandbox session. The session keeps a Docker container running so you can execute multiple commands, write files, and iterate. Returns a session_id to use with other session tools. Available sandboxes: " +
		strings.Join(sandboxIDs, ", ") + "."

	return mcp.NewTool("session_create",
		mcp.WithDescription(desc),
		mcp.WithString("sandbox",
			mcp.Required(),
			mcp.Description("The sandbox environment to use (e.g. python, go, javascript)."),
		),
	)
}

// NewSessionExecTool creates the session_exec MCP tool definition.
func NewSessionExecTool() mcp.Tool {
	return mcp.NewTool("session_exec",
		mcp.WithDescription("Execute a shell command in a running session. The command is run via `sh -c` in the container's working directory. Returns stdout and stderr."),
		mcp.WithString("session_id",
			mcp.Required(),
			mcp.Description("The session ID returned by session_create."),
		),
		mcp.WithString("command",
			mcp.Required(),
			mcp.Description("The shell command to execute."),
		),
		mcp.WithNumber("timeout_seconds",
			mcp.Description("Execution timeout in seconds. Defaults to 30, max 300."),
		),
	)
}

// NewSessionWriteFileTool creates the session_write_file MCP tool definition.
func NewSessionWriteFileTool() mcp.Tool {
	return mcp.NewTool("session_write_file",
		mcp.WithDescription("Write a file into the session's working directory. The file is immediately available in the container. Paths are relative to the working directory; absolute paths and '..' are rejected."),
		mcp.WithString("session_id",
			mcp.Required(),
			mcp.Description("The session ID returned by session_create."),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("File path relative to the working directory (e.g. 'main.py', 'src/lib.rs')."),
		),
		mcp.WithString("content",
			mcp.Required(),
			mcp.Description("The file content to write."),
		),
	)
}

// NewSessionReadFileTool creates the session_read_file MCP tool definition.
func NewSessionReadFileTool() mcp.Tool {
	return mcp.NewTool("session_read_file",
		mcp.WithDescription("Read a file from the session. Reads from the working directory by default. Optionally copies the file to a host path for artifact extraction. File size is limited to 50MB."),
		mcp.WithString("session_id",
			mcp.Required(),
			mcp.Description("The session ID returned by session_create."),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("File path relative to the working directory (e.g. 'output.txt', 'build/app')."),
		),
		mcp.WithString("host_path",
			mcp.Description("Optional absolute path on the host to copy the file to for artifact extraction."),
		),
	)
}

// NewSessionDestroyTool creates the session_destroy MCP tool definition.
func NewSessionDestroyTool() mcp.Tool {
	return mcp.NewTool("session_destroy",
		mcp.WithDescription("Destroy a session, removing its container and temporary files. The session_id becomes invalid after this call."),
		mcp.WithString("session_id",
			mcp.Required(),
			mcp.Description("The session ID returned by session_create."),
		),
	)
}
