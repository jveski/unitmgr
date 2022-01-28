package main

import (
	"io/ioutil"
	"os"
	"path"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunLoop(t *testing.T) {
	watcher, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	defer watcher.Close()

	dir := t.TempDir()
	err = watcher.Add(dir)
	require.NoError(t, err)

	n := 0
	runLoop(watcher, func() time.Duration {
		n++
		switch n {
		case 1: // initial resync
			err := ioutil.WriteFile(path.Join(dir, "test1"), []byte("test1"), 0644)
			require.NoError(t, err)
			return time.Hour
		case 2: // file changed
			return time.Nanosecond
		case 3: // resync
			watcher.Close()
		}
		return time.Hour
	})
}

func TestSync(t *testing.T) {
	src := t.TempDir()
	dest := t.TempDir()
	state := map[string]string{}
	sysd := &fakeSystemd{}

	t.Run("zero units", func(t *testing.T) {
		assert.True(t, sync(src, dest, state, sysd))
	})

	t.Run("create unit", func(t *testing.T) {
		err := ioutil.WriteFile(path.Join(src, "test1.service"), []byte("test1"), 0644)
		require.NoError(t, err)

		assert.True(t, sync(src, dest, state, sysd))
		assert.FileExists(t, path.Join(dest, "test1.service"))
		assert.Equal(t, "EnsureRunning test1.service", sysd.LastCmd)
	})

	t.Run("sync unit no change", func(t *testing.T) {
		assert.True(t, sync(src, dest, state, sysd))
		assert.FileExists(t, path.Join(dest, "test1.service"))
	})

	t.Run("change unit", func(t *testing.T) {
		err := ioutil.WriteFile(path.Join(src, "test1.service"), []byte("test2"), 0644)
		require.NoError(t, err)

		assert.True(t, sync(src, dest, state, sysd))
		assert.FileExists(t, path.Join(dest, "test1.service"))
		assert.Equal(t, "Restart test1.service", sysd.LastCmd)
	})

	t.Run("remove unit", func(t *testing.T) {
		err := os.Remove(path.Join(src, "test1.service"))
		require.NoError(t, err)

		assert.True(t, sync(src, dest, state, sysd))
		assert.NoFileExists(t, path.Join(dest, "test1.service"))
		assert.Equal(t, "EnsureStopped test1.service", sysd.LastCmd)
	})
}

type fakeSystemd struct {
	LastCmd string
}

func (f *fakeSystemd) Restart(unit string) error {
	f.LastCmd = "Restart " + unit
	return nil
}

func (f *fakeSystemd) EnsureRunning(unit string) (bool, error) {
	f.LastCmd = "EnsureRunning " + unit
	return false, nil
}

func (f *fakeSystemd) EnsureStopped(unit string) (bool, error) {
	f.LastCmd = "EnsureStopped " + unit
	return false, nil
}
