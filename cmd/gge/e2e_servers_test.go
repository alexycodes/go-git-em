package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/crypto/ssh"
)

// startServers serves the repositories under $WORK/srv over HTTP, HTTPS and
// SSH, for as long as the script runs. git runs on the server side with
// gitEnv. The servers log to $WORK/servers.log.
func startServers(env *testscript.Env, git string, gitEnv []string) error {
	logFile, err := os.Create(filepath.Join(env.WorkDir, "servers.log"))
	if err != nil {
		return err
	}
	env.Defer(func() { _ = logFile.Close() }) // deferred calls run last-in first-out: after the servers stop
	root := filepath.Join(env.WorkDir, "srv")
	if err := startHTTP(env, git, gitEnv, root, logFile); err != nil {
		return err
	}
	return startSSH(env, git, gitEnv, root, logFile)
}

// startHTTP serves smart HTTP with git-http-backend, as $GITHTTP and, with
// the CA in $WORK/ca.pem, as $GITHTTPS. Under /private/ the servers ask for
// credentials, and under /hang/ they log the request and never answer.
func startHTTP(env *testscript.Env, git string, gitEnv []string, root string, logw io.Writer) error {
	execPath, err := exec.Command(git, "--exec-path").Output()
	if err != nil {
		return fmt.Errorf("git --exec-path: %w", err)
	}
	quit := make(chan struct{})
	logger := log.New(logw, "http: ", 0)
	mux := http.NewServeMux()
	mux.HandleFunc("/private/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/hang/", func(_ http.ResponseWriter, r *http.Request) {
		logger.Printf("hang: %s %s", r.Method, r.URL.Path) // so a script can wait for the request
		select {
		case <-r.Context().Done():
		case <-quit: // Close waits for every request to finish
		}
	})
	mux.Handle("/", &cgi.Handler{
		Path:   filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Env:    append([]string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}, gitEnv...),
		Logger: logger,
		Stderr: logw,
	})

	// The log keeps errors such as a rejected certificate out of the test's
	// output.
	plain := httptest.NewUnstartedServer(mux)
	plain.Config.ErrorLog = logger
	plain.Start()
	secure := httptest.NewUnstartedServer(mux)
	secure.Config.ErrorLog = logger
	secure.StartTLS()
	env.Defer(func() {
		close(quit)
		plain.Close()
		secure.Close()
	})

	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: secure.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(env.WorkDir, "ca.pem"), ca, 0o644); err != nil {
		return err
	}
	env.Setenv("GITHTTP", plain.URL)
	env.Setenv("GITHTTPS", secure.URL)
	return nil
}

// startSSH serves git-upload-pack over SSH as $GITSSH (ssh://git@127.0.0.1:PORT)
// and as the scp-style host gitserver.test, to the one client key it accepts,
// $SSHKEY. $GIT_SSH_COMMAND points OpenSSH at $WORK/ssh_config, which trusts the
// server through $WORK/known_hosts: OpenSSH reads ~/.ssh from the passwd
// entry, not from $HOME.
func startSSH(env *testscript.Env, git string, gitEnv []string, root string, logw io.Writer) error {
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		return err
	}
	clientPublic, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	authorized, err := ssh.NewPublicKey(clientPublic)
	if err != nil {
		return err
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return nil, errors.New("unknown key")
			}
			return nil, nil
		},
	}
	config.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return fmt.Errorf("listening on %v, not TCP", ln.Addr())
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &sshServer{
		git:    git,
		root:   root,
		env:    gitEnv,
		config: config,
		log:    log.New(logw, "ssh: ", 0),
		ln:     ln,
		ctx:    ctx,
		cancel: cancel,
		conns:  make(map[net.Conn]bool),
	}
	s.wg.Go(s.accept)
	env.Defer(s.close)

	block, err := ssh.MarshalPrivateKey(clientKey, "")
	if err != nil {
		return err
	}
	keyPath := filepath.Join(env.WorkDir, "id_ed25519")
	knownHosts := filepath.Join(env.WorkDir, "known_hosts")
	sshConfig := filepath.Join(env.WorkDir, "ssh_config")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		return err
	}
	hostLine := fmt.Sprintf("[127.0.0.1]:%d %s", addr.Port, ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))
	if err := os.WriteFile(knownHosts, []byte(hostLine), 0o644); err != nil {
		return err
	}
	// The alias gives scp-style URLs, which can't carry a port, a way to the
	// server. OpenSSH looks the alias up in known_hosts by its HostName.
	// StrictHostKeyChecking keeps OpenSSH's default, ask, which users get too.
	cfg := fmt.Sprintf(`Host gitserver.test
	HostName 127.0.0.1
	Port %d
Host *
	UserKnownHostsFile "%s"
	GlobalKnownHostsFile /dev/null
`, addr.Port, knownHosts)
	if err := os.WriteFile(sshConfig, []byte(cfg), 0o644); err != nil {
		return err
	}
	env.Setenv("GITSSH", fmt.Sprintf("ssh://git@127.0.0.1:%d", addr.Port))
	env.Setenv("SSHKEY", keyPath)
	env.Setenv("GIT_SSH_COMMAND", "ssh -F '"+sshConfig+"'")
	return nil
}

// sshServer runs git-upload-pack for the repositories under root. Anything
// else a client asks for fails.
type sshServer struct {
	git    string
	root   string
	env    []string
	config *ssh.ServerConfig
	log    *log.Logger
	ln     net.Listener

	ctx    context.Context // ends the git processes when the server closes
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	conns  map[net.Conn]bool
	closed bool
}

func (s *sshServer) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // the listener is closed
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = true
		s.mu.Unlock()
		s.wg.Go(func() { s.serve(conn) })
	}
}

// close stops the server, and waits until every connection and git process
// it started has ended.
func (s *sshServer) close() {
	s.cancel()
	_ = s.ln.Close()
	s.mu.Lock()
	s.closed = true
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *sshServer) serve(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()
	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		s.log.Printf("handshake: %v", err)
		return
	}
	defer func() { _ = sconn.Close() }()
	s.wg.Go(func() { ssh.DiscardRequests(reqs) })
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			s.log.Printf("accept a session: %v", err)
			continue
		}
		s.wg.Go(func() { s.session(ch, chReqs) })
	}
}

// session handles a session's requests: GIT_PROTOCOL from env requests, so
// that git can use protocol v2, and one exec request.
func (s *sshServer) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	var env []string
	started := false
	for req := range reqs {
		switch req.Type {
		case "env":
			var kv struct{ Name, Value string }
			ok := ssh.Unmarshal(req.Payload, &kv) == nil && kv.Name == "GIT_PROTOCOL"
			if ok {
				env = append(env, "GIT_PROTOCOL="+kv.Value)
			}
			_ = req.Reply(ok, nil)
		case "exec":
			var cmd struct{ Command string }
			if started || ssh.Unmarshal(req.Payload, &cmd) != nil {
				_ = req.Reply(false, nil)
				continue
			}
			started = true
			_ = req.Reply(true, nil)
			cmdEnv := slices.Clone(env)
			s.wg.Go(func() {
				status := s.run(ch, cmd.Command, cmdEnv)
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
				_ = ch.Close()
			})
		default:
			_ = req.Reply(false, nil)
		}
	}
}

// run runs git-upload-pack '<path>' for a path under root, and returns its
// exit status. Anything else exits 127, as an unknown command in a shell does.
func (s *sshServer) run(ch ssh.Channel, command string, env []string) uint32 {
	path, ok := strings.CutPrefix(command, "git-upload-pack '")
	path, quoted := strings.CutSuffix(path, "'")
	// ssh:// URLs give an absolute path, and scp-style ones a relative path.
	rel := strings.TrimPrefix(path, "/")
	if !ok || !quoted || strings.Contains(path, "'") || !filepath.IsLocal(rel) {
		fmt.Fprintf(ch.Stderr(), "unsupported command: %s\n", command)
		return 127
	}
	cmd := exec.CommandContext(s.ctx, s.git, "upload-pack", filepath.Join(s.root, rel))
	cmd.Env = append(slices.Clone(s.env), env...)
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	// Bounds the wait for a client that keeps its side open after git exits.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		s.log.Printf("%s: %v", command, err)
	}
	code := cmd.ProcessState.ExitCode()
	if code < 0 { // killed, or never started
		code = 128
	}
	return uint32(code)
}
