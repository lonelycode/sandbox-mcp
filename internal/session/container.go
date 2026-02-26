package session

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/client"
)

// generateSessionID creates a random 32-character hex string from 16 random bytes.
func generateSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate session ID: %v", err)
	}
	return hex.EncodeToString(b), nil
}

// sanitizePath cleans a file path and rejects absolute paths and directory traversal.
// It returns the cleaned path relative to the working directory.
func sanitizePath(path string) (string, error) {
	cleaned := filepath.Clean(path)

	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute paths are not allowed: %s", path)
	}

	if strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("path traversal is not allowed: %s", path)
	}

	return cleaned, nil
}

// extractSingleFileFromTar reads a tar stream and returns the content of the first regular file.
func extractSingleFileFromTar(r io.Reader, maxSize int64) ([]byte, error) {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("file not found in tar archive")
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar archive: %v", err)
		}

		if header.Typeflag == tar.TypeReg {
			if header.Size > maxSize {
				return nil, fmt.Errorf("file size %d bytes exceeds limit of %d bytes", header.Size, maxSize)
			}
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, io.LimitReader(tr, maxSize+1)); err != nil {
				return nil, fmt.Errorf("failed to read file from tar: %v", err)
			}
			if int64(buf.Len()) > maxSize {
				return nil, fmt.Errorf("file size exceeds limit of %d bytes", maxSize)
			}
			return buf.Bytes(), nil
		}
	}
}

// waitForContainer waits for a container to be in running state with a specified timeout.
func waitForContainer(ctx context.Context, cli *client.Client, containerID string, timeout time.Duration) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	timeoutCh := time.After(timeout)

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while waiting for container to start")
		case <-timeoutCh:
			return fmt.Errorf("container did not reach running state within %v", timeout)
		case <-ticker.C:
			inspect, err := cli.ContainerInspect(ctx, containerID)
			if err != nil {
				return fmt.Errorf("failed to inspect container: %v", err)
			}
			if inspect.State != nil && inspect.State.Running {
				return nil
			}
		}
	}
}
