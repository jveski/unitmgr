package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

func main() {
	var (
		src     = flag.String("src", ".", "path to directory containing your unit files")
		dest    = flag.String("dest", "/etc/systemd/system", "path to systemd's unit file directory")
		resync  = flag.Duration("resync", time.Hour, "how often to check for unit file consistency")
		retry   = flag.Duration("retry", time.Second, "how often to retry failed operations")
		timeout = flag.Duration("timeout", time.Second*10, "timeout for systemctl operations")
	)
	flag.Parse()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err)
	}
	defer watcher.Close()

	if err := os.MkdirAll(*src, 0755); err != nil {
		panic(err)
	}

	err = watcher.Add(*src)
	if err != nil {
		panic(err)
	}

	state := map[string]string{}
	sysd := &systemctl{Timeout: *timeout}
	err = runLoop(watcher, func() time.Duration {
		if sync(*src, *dest, state, sysd) {
			return *resync
		}
		return *retry
	})
	if err != nil {
		panic(err)
	}
}

func runLoop(watcher *fsnotify.Watcher, fn func() time.Duration) error {
	ticker := time.NewTimer(1)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ticker.Reset(fn())
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			switch event.Op {
			case fsnotify.Write, fsnotify.Create, fsnotify.Remove, fsnotify.Rename:
				ticker.Reset(fn())
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			return fmt.Errorf("watcher error: %w", err)
		}
	}
}

func sync(src, dest string, state map[string]string, sysd systemd) bool {
	files, err := ioutil.ReadDir(src)
	if err != nil {
		log.Printf("error while listing unit files: %s", err)
		return false
	}

	ok := true
	for _, stat := range files {
		if strings.HasSuffix(stat.Name(), ".swp") || strings.HasSuffix(stat.Name(), "~") {
			continue // skip vim files
		}

		unit := path.Base(stat.Name())
		name := path.Join(src, unit)

		checksum, err := getChecksum(name)
		if err != nil {
			log.Printf("error reading unit file %q: %s", unit, err)
			ok = false
			continue
		}
		if os.IsNotExist(err) {
			continue // file was removed between the time of the notification and now
		}

		target := path.Join(dest, unit)
		currentChecksum, err := getChecksum(target)
		if err != nil && !os.IsNotExist(err) {
			log.Printf("error reading current unit file %q: %s", unit, err)
			ok = false
			continue
		}

		// Make sure the unit file is in sync
		if checksum != currentChecksum {
			if err := copyFile(name, target); err != nil {
				log.Printf("error while copying unit file %q: %s", unit, err)
				ok = false
				continue
			}
			log.Printf("wrote unit: %s", unit)
		}

		// Make sure unit is running if it's new or already in the correct state
		if checksum == currentChecksum || currentChecksum == "" {
			changed, err := sysd.EnsureRunning(unit)
			if err != nil {
				log.Printf("error while ensuring unit %q is running: %s", unit, err)
				ok = false
				continue
			}
			if changed {
				log.Printf("started unit: %s", unit)
			}
			state[unit] = checksum
			continue
		}

		// Restart units when their last configuration doesn't match the current one
		if checksum != state[unit] {
			err = sysd.Restart(unit)
			if err != nil {
				log.Printf("error while restarting unit %q: %s", unit, err)
				ok = false
				continue
			}
			log.Printf("restarted unit: %s", unit)
			state[unit] = checksum
		}
	}

	for unit := range state {
		if _, err := os.Stat(path.Join(src, unit)); err == nil {
			continue // file still exists
		}

		changed, err := sysd.EnsureStopped(unit)
		if err != nil {
			log.Printf("error while stopping unit %q: %s", unit, err)
			ok = false
			continue
		}
		if changed {
			log.Printf("stopped unit: %s", unit)
		}

		target := path.Join(dest, unit)
		if err := os.Remove(target); err != nil {
			log.Printf("error while removing unit %q: %s", unit, err)
			ok = false
			continue
		}
		log.Printf("removed unit: %s", unit)

		delete(state, unit)
	}

	return ok
}

func getChecksum(name string) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dest string) error {
	srcf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcf.Close()

	destf, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer destf.Close()

	_, err = io.Copy(destf, srcf)
	return err
}

type systemd interface {
	Restart(unit string) error
	EnsureRunning(unit string) (bool, error)
	EnsureStopped(unit string) (bool, error)
}

type systemctl struct {
	Timeout time.Duration
}

func (s *systemctl) Restart(unit string) error {
	ctx, done := context.WithTimeout(context.Background(), s.Timeout)
	defer done()

	if err := s.exec(ctx, "daemon-reload"); err != nil {
		return err
	}

	return s.exec(ctx, "restart", unit)
}

func (s *systemctl) EnsureRunning(unit string) (bool, error) {
	ctx, done := context.WithTimeout(context.Background(), s.Timeout)
	defer done()

	if s.isRunning(ctx, unit) {
		return false, nil // already running
	}

	return true, s.exec(ctx, "restart", unit)
}

func (s *systemctl) EnsureStopped(unit string) (bool, error) {
	ctx, done := context.WithTimeout(context.Background(), s.Timeout)
	defer done()

	if !s.isRunning(ctx, unit) {
		return false, nil // already stopped
	}

	return true, s.exec(ctx, "stop", unit)
}

func (s *systemctl) isRunning(ctx context.Context, unit string) bool {
	return exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit).Run() == nil
}

func (s *systemctl) exec(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err == nil {
		return nil
	}
	if len(out) > 0 {
		return fmt.Errorf("systemctl error msg: %s", out)
	}
	return fmt.Errorf("systemctl error: %w", err)
}
