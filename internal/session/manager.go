package session

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/pottekkat/sandbox-mcp/internal/config"
)

const (
	idleTimeout    = 15 * time.Minute
	maxLifetime    = 2 * time.Hour
	reaperInterval = 30 * time.Second
)

// Session represents a persistent sandbox session with a running container.
type Session struct {
	ID           string
	ContainerID  string
	Client       *client.Client
	TempDir      string
	Config       *config.SandboxConfig
	CreatedAt    time.Time
	LastActivity time.Time

	mu sync.Mutex // serializes exec calls and activity updates
}

// touch updates the last activity timestamp.
func (s *Session) touch() {
	s.LastActivity = time.Now()
}

// SessionManager manages the lifecycle of persistent sandbox sessions.
type SessionManager struct {
	configs map[string]*config.SandboxConfig

	mu       sync.RWMutex
	sessions map[string]*Session

	stopReaper chan struct{}
}

// NewSessionManager creates a new session manager and starts the background reaper.
func NewSessionManager(configs map[string]*config.SandboxConfig) *SessionManager {
	m := &SessionManager{
		configs:    configs,
		sessions:   make(map[string]*Session),
		stopReaper: make(chan struct{}),
	}
	go m.reaper()
	return m
}

// Create spins up a new persistent session for the given sandbox ID.
func (m *SessionManager) Create(ctx context.Context, sandboxID string) (*Session, error) {
	sandboxCfg, ok := m.configs[sandboxID]
	if !ok {
		available := make([]string, 0, len(m.configs))
		for id := range m.configs {
			available = append(available, id)
		}
		return nil, fmt.Errorf("unknown sandbox %q, available: %v", sandboxID, available)
	}

	sessionID, err := generateSessionID()
	if err != nil {
		return nil, err
	}

	// Create temp directory for the session (bind-mounted into the container)
	dir, err := os.MkdirTemp("", sandboxCfg.Mount.TmpDirPrefix+"session-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %v", err)
	}

	// Create Docker client
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to create Docker client: %v", err)
	}

	// Container config: sleep infinity to keep it alive
	containerConfig := &container.Config{
		Image:      sandboxCfg.Image,
		Cmd:        []string{"sleep", "infinity"},
		WorkingDir: sandboxCfg.Mount.WorkDir,
		User:       sandboxCfg.User,
	}

	// Host config: same security model as ephemeral sandboxes
	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			Memory:    sandboxCfg.Resources.Memory * 1024 * 1024,
			NanoCPUs:  int64(sandboxCfg.Resources.CPU * 1e9),
			PidsLimit: &sandboxCfg.Resources.Processes,
			Ulimits: []*container.Ulimit{
				{
					Name: "nofile",
					Soft: sandboxCfg.Resources.Files,
					Hard: sandboxCfg.Resources.Files,
				},
			},
		},
		NetworkMode:    container.NetworkMode(sandboxCfg.Security.Network),
		ReadonlyRootfs: sandboxCfg.Security.ReadOnly,
		Mounts: []mount.Mount{
			{
				Type:     mount.TypeBind,
				Source:   dir,
				Target:   sandboxCfg.Mount.WorkDir,
				ReadOnly: sandboxCfg.Mount.ReadOnly,
			},
		},
		CapDrop:     sandboxCfg.Security.CapDrop,
		SecurityOpt: sandboxCfg.Security.SecurityOpt,
	}

	resp, err := cli.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		cli.Close()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to create container: %v", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up the created container
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = cli.ContainerRemove(cleanupCtx, resp.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		cli.Close()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to start container: %v", err)
	}

	// Wait for the container to reach running state
	if err := waitForContainer(ctx, cli, resp.ID, 10*time.Second); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = cli.ContainerRemove(cleanupCtx, resp.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		cli.Close()
		os.RemoveAll(dir)
		return nil, err
	}

	now := time.Now()
	sess := &Session{
		ID:           sessionID,
		ContainerID:  resp.ID,
		Client:       cli,
		TempDir:      dir,
		Config:       sandboxCfg,
		CreatedAt:    now,
		LastActivity: now,
	}

	m.mu.Lock()
	m.sessions[sessionID] = sess
	m.mu.Unlock()

	return sess, nil
}

// Get returns the session for the given ID, or an error if not found.
func (m *SessionManager) Get(sessionID string) (*Session, error) {
	m.mu.RLock()
	sess, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("session %q not found (it may have expired or been destroyed)", sessionID)
	}
	return sess, nil
}

// Destroy tears down a session: force-removes the container, deletes temp files.
func (m *SessionManager) Destroy(sessionID string) error {
	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("session %q not found (it may have expired or been destroyed)", sessionID)
	}

	return m.cleanup(sess)
}

// DestroyAll tears down all sessions. Called during graceful shutdown.
func (m *SessionManager) DestroyAll() {
	close(m.stopReaper)

	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sessions = append(sessions, sess)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	for _, sess := range sessions {
		if err := m.cleanup(sess); err != nil {
			log.Printf("Failed to clean up session %s: %v", sess.ID, err)
		}
	}
}

// cleanup force-removes the container, closes the Docker client, and deletes the temp dir.
func (m *SessionManager) cleanup(sess *Session) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := sess.Client.ContainerRemove(ctx, sess.ContainerID, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})

	sess.Client.Close()
	os.RemoveAll(sess.TempDir)

	return err
}

// reaper periodically checks for expired sessions and cleans them up.
func (m *SessionManager) reaper() {
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopReaper:
			return
		case <-ticker.C:
			m.reapExpired()
		}
	}
}

// reapExpired finds and destroys sessions that exceed idle timeout or max lifetime.
func (m *SessionManager) reapExpired() {
	now := time.Now()

	m.mu.Lock()
	var expired []*Session
	for id, sess := range m.sessions {
		sess.mu.Lock()
		idle := now.Sub(sess.LastActivity) > idleTimeout
		tooOld := now.Sub(sess.CreatedAt) > maxLifetime
		sess.mu.Unlock()

		if idle || tooOld {
			expired = append(expired, sess)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()

	for _, sess := range expired {
		log.Printf("Reaping expired session %s (sandbox=%s)", sess.ID, sess.Config.Id)
		if err := m.cleanup(sess); err != nil {
			log.Printf("Failed to clean up expired session %s: %v", sess.ID, err)
		}
	}
}
