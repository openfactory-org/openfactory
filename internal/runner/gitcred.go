package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const sessionFileName = "session.json"

// sessionFile is the content of StateDir/session.json. The git credential
// helper reads it to find the control plane and the session token.
type sessionFile struct {
	ControlURL    string `json:"control_url"`
	SessionToken  string `json:"session_token"`
	RunnerVersion string `json:"runner_version"`
}

// saveSession writes session.json with the current session token. It writes
// a temporary file first, so that the helper never reads a partial file.
func (r *runner) saveSession() error {
	data, err := json.Marshal(sessionFile{
		ControlURL:    r.cfg.ControlURL,
		SessionToken:  r.client.Token(),
		RunnerVersion: r.cfg.Version,
	})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.cfg.StateDir, sessionFileName+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, filepath.Join(r.cfg.StateDir, sessionFileName)); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// keepSession writes session.json and logs a failure. A failure only
// disables git credentials, so it does not stop the session.
func (r *runner) keepSession() {
	if err := r.saveSession(); err != nil {
		r.log.Warn("write session file", "error", err)
	}
}

// GitCredential is the git credential helper. For op "get" it reads the
// request of git from in and writes the username and the password to out.
// For "store" and "erase" it reads the input and does nothing else.
func GitCredential(ctx context.Context, stateDir string, op string, in io.Reader, out io.Writer) error {
	attrs, err := readCredentialInput(in)
	if err != nil {
		return err
	}
	if op != "get" {
		return nil
	}
	sf, err := loadSession(stateDir)
	if err != nil {
		return err
	}
	c, err := runnerclient.New(runnerclient.Options{BaseURL: sf.ControlURL, RunnerVersion: sf.RunnerVersion})
	if err != nil {
		return err
	}
	c.SetToken(sf.SessionToken)
	resp, err := c.GitCredential(ctx, runnerproto.GitCredentialRequest{
		Protocol: attrs["protocol"],
		Host:     attrs["host"],
		Path:     attrs["path"],
	})
	if err != nil {
		return fmt.Errorf("git credential: %w", err)
	}
	_, err = fmt.Fprintf(out, "username=%s\npassword=%s\n", resp.Username, resp.Password)
	return err
}

// readCredentialInput reads key=value lines until an empty line or EOF.
func readCredentialInput(in io.Reader) (map[string]string, error) {
	attrs := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			attrs[k] = v
		}
	}
	return attrs, sc.Err()
}

func loadSession(stateDir string) (sessionFile, error) {
	var sf sessionFile
	data, err := os.ReadFile(filepath.Join(stateDir, sessionFileName))
	if err != nil {
		return sf, fmt.Errorf("git credential: read session file: %w", err)
	}
	if err := json.Unmarshal(data, &sf); err != nil {
		return sf, fmt.Errorf("git credential: decode session file: %w", err)
	}
	if sf.ControlURL == "" || sf.SessionToken == "" {
		return sf, errors.New("git credential: the session file has no control URL or token")
	}
	return sf, nil
}
