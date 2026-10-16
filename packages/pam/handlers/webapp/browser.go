package webapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/rs/zerolog/log"
)

// BrowserPathEnv overrides where the gateway looks for a Chromium-based browser.
const BrowserPathEnv = "INFISICAL_PAM_BROWSER_PATH"

const defaultBrowserPath = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

// FindBrowser returns the browser binary the gateway will launch, or an error if none is installed.
func FindBrowser() (string, error) {
	if path := os.Getenv(BrowserPathEnv); path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("%s points to %q, which does not exist", BrowserPathEnv, path)
		}
		return path, nil
	}
	if _, err := os.Stat(defaultBrowserPath); err != nil {
		return "", fmt.Errorf("no Chromium-based browser found at %q; install Google Chrome or set %s", defaultBrowserPath, BrowserPathEnv)
	}
	return defaultBrowserPath, nil
}

// IsSupported reports whether this gateway can serve web app sessions, so it only advertises the
// resource type when a session could actually start (the same rule RDP follows).
func IsSupported() bool {
	_, err := FindBrowser()
	return err == nil
}

type browser struct {
	cmd        *exec.Cmd
	cdp        *cdpClient
	profileDir string
	exited     chan struct{}
}

func launchBrowser(width, height int) (*browser, error) {
	path, err := FindBrowser()
	if err != nil {
		return nil, err
	}

	// A fresh profile per session: no cookies, cache or history survive the session or leak between users.
	profileDir, err := os.MkdirTemp("", "infisical-pam-webapp-")
	if err != nil {
		return nil, fmt.Errorf("failed to create browser profile directory: %w", err)
	}

	// Chrome reads commands from fd 3 and writes responses to fd 4.
	cmdReader, cmdWriter, err := os.Pipe()
	if err != nil {
		os.RemoveAll(profileDir)
		return nil, err
	}
	respReader, respWriter, err := os.Pipe()
	if err != nil {
		cmdReader.Close()
		cmdWriter.Close()
		os.RemoveAll(profileDir)
		return nil, err
	}

	// Chrome's own sandbox stays on: --no-sandbox is deliberately absent.
	cmd := exec.Command(path,
		"--headless=new",
		"--remote-debugging-pipe",
		"--user-data-dir="+profileDir,
		fmt.Sprintf("--window-size=%d,%d", width, height),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-sync",
		"--mute-audio",
		"about:blank",
	)
	cmd.ExtraFiles = []*os.File{cmdReader, respWriter}

	if err := cmd.Start(); err != nil {
		cmdReader.Close()
		cmdWriter.Close()
		respReader.Close()
		respWriter.Close()
		os.RemoveAll(profileDir)
		return nil, fmt.Errorf("failed to start browser: %w", err)
	}
	// The child holds its own copies of these ends.
	cmdReader.Close()
	respWriter.Close()

	b := &browser{
		cmd:        cmd,
		cdp:        newCDPClient(respReader, cmdWriter),
		profileDir: profileDir,
		exited:     make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		cmdWriter.Close()
		respReader.Close()
		close(b.exited)
	}()

	log.Debug().Str("browser", path).Int("pid", cmd.Process.Pid).Msg("Started web app browser")
	return b, nil
}

// close asks the browser to exit, kills it if it doesn't, and removes its profile.
func (b *browser) close() {
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = b.cdp.call(closeCtx, "", "Browser.close", nil, nil)
	cancel()

	select {
	case <-b.exited:
	case <-time.After(3 * time.Second):
		if err := b.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			log.Warn().Err(err).Msg("Failed to kill web app browser")
		}
		<-b.exited
	}
	b.cdp.close()

	if err := os.RemoveAll(b.profileDir); err != nil {
		log.Warn().Err(err).Str("profileDir", b.profileDir).Msg("Failed to remove web app browser profile")
	}
}
