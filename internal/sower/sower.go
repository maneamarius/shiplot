/*
Copyright © 2023 Taylor Paddock

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package sower

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/panjf2000/ants/v2"
	"github.com/tcpaddock/shiplot/internal/config"
	"golang.org/x/exp/slog"
)

type Sower struct {
	ctx      context.Context
	cancel   context.CancelFunc
	cfg      config.Config
	paths    *pathList
	movePool *ants.PoolWithFunc
	watcher  *fsnotify.Watcher
	wg       sync.WaitGroup
}

func NewSower(ctx context.Context, cfg config.Config) (s *Sower, err error) {
	s = new(Sower)

	s.ctx, s.cancel = context.WithCancel(ctx)
	s.cfg = cfg

	// Fill list of available destination paths
	s.paths = new(pathList)
	s.paths.Populate(s.cfg.DestinationPaths)

	// Create worker pool for moving plots
	size := s.getPoolSize()
	slog.Default().Info(fmt.Sprintf("Creating worker pool with max size %d", size))
	s.movePool, err = ants.NewPoolWithFunc(size, func(i interface{}) {
		s.movePlot(i)
		s.wg.Done()
	})
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Sower) Run() (err error) {
	// Create filesystem watcher
	s.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// Start running watcher
	s.wg.Add(1)
	go s.runLoop()

	// Add staging path to watcher
	slog.Default().Info(fmt.Sprintf("Starting watcher on %s", s.cfg.StagingPath))
	err = s.watcher.Add(s.cfg.StagingPath)
	if err != nil {
		s.Close()
		return err
	}

	// Move existing plots
	files, err := os.ReadDir(s.cfg.StagingPath)
	if err != nil {
		return err
	}

	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".fpt") {
			s.wg.Add(1)
			err = s.movePool.Invoke(filepath.Join(s.cfg.StagingPath, file.Name()))
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Sower) Close() {
	s.cancel()
	s.wg.Wait()
}

func (s *Sower) runLoop() {
	defer s.wg.Done()
	for {
		select {
		// Read from Errors
		case err, ok := <-s.watcher.Errors:
			if !ok { // Channel was closed (i.e. Watcher.Close() was called)
				return
			}
			slog.Default().Error("File watcher error", err)
		// Read from Events
		case e, ok := <-s.watcher.Events:
			if !ok { // Channel was closed (i.e. Watcher.Close() was called)
				return
			}

			if e.Op.Has(fsnotify.Create) && strings.HasSuffix(e.Name, ".fpt") {
				s.wg.Add(1)
				err := s.movePool.Invoke(e.Name)
				if err != nil {
					slog.Default().Error(fmt.Sprintf("Failed to invoke job for %s", e.Name), err)
				}
			}
		// Read from context for closing
		case <-s.ctx.Done():
			s.watcher.Close()
			s.movePool.Release()
			return
		}
	}
}


func (s *Sower) movePlot(i interface{}) {
	var (
		srcFullName = i.(string)
		srcName     = filepath.Base(srcFullName)
	)

	// Open source file
	src, err := os.Open(srcFullName)
	if err != nil {
		src.Close()
		slog.Default().Error(fmt.Sprintf("Failed to open %s", srcFullName), err)
		return
	}

	// Get source file size
	srcInfo, err := src.Stat()
	if err != nil {
		src.Close()
		slog.Default().Error(fmt.Sprintf("Failed to get the file size of %s", srcFullName), err)
		return
	}

	// Find the best destination path
	var dstPath *path

	for {
		// Gets the lowest sized first path and marks it unavailable
		dstPath = s.paths.FirstAvailable()

		// Wait for 10 seconds if no available destination
		if dstPath == nil {
			time.Sleep(time.Second * 10)
			continue
		}

		// Ensure destination path has enough space
		if uint64(srcInfo.Size()) < dstPath.usage.Free() {
			break
		} else {
			// Remove path if space is too low
			s.paths.Remove(dstPath)

			// Adjust move pool
			size := s.getPoolSize()
			if s.movePool.Cap() != size {
				slog.Default().Info(fmt.Sprintf("Adjusting worker pool max size to %d", size))
				s.movePool.Tune(size)
			}
			continue
		}
	}

	var (
		dstDir      = dstPath.name
		dstFullName = filepath.Join(dstDir, srcName)
	)

	slog.Default().Info(fmt.Sprintf("Moving %s to %s", srcName, dstDir))

	// Open destination file
	dst, err := os.Create(dstFullName + ".tmp")
	if err != nil {
		src.Close()
		dst.Close()
		s.paths.Update(dstPath, true)
		slog.Default().Error(fmt.Sprintf("Failed to create temp file %s", dstFullName+".tmp"), err)
		return
	}

	start := time.Now()

	// Copy plot to temporary file
	written, err := io.Copy(dst, src)
	if err != nil {
		src.Close()
		dst.Close()
		s.paths.Update(dstPath, true)
		slog.Default().Error(fmt.Sprintf("Failed to copy %s to %s", srcFullName, dstFullName+".tmp"), err)
		return
	}

	// Rename temporary file
	err = os.Rename(dstFullName+".tmp", dstFullName)
	if err != nil {
		src.Close()
		dst.Close()
		s.paths.Update(dstPath, true)
		slog.Default().Error(fmt.Sprintf("Failed to rename %s to %s", dstFullName+".tmp", dstFullName), err)
		return
	}

	duration := time.Since(start)

	// Close source file
	err = src.Close()
	if err != nil {
		s.paths.Update(dstPath, true)
		slog.Default().Error(fmt.Sprintf("Failed to close %s", srcFullName), err)
		return
	}

	// Close destination file
	err = dst.Close()
	if err != nil {
		s.paths.Update(dstPath, true)
		slog.Default().Error(fmt.Sprintf("Failed to close %s", dstFullName), err)
		return
	}

	// Move succeeded, delete source
	err = os.Remove(src.Name())
	if err != nil {
		slog.Default().Error(fmt.Sprintf("Failed to delete %s", src.Name()), err)
		return
	}

	// Update available paths
	s.paths.Update(dstPath, true)

	slog.Default().Info(fmt.Sprintf("Moved %s to %s", srcName, dstDir), slog.Int64("written", written), slog.Duration("time", duration))
}

func (s *Sower) getPoolSize() (size int) {
	poolSize := int(s.cfg.MaxThreads)

	if poolSize == 0 {
		poolSize = s.paths.Len()
	} else {
		if s.paths.Len() < poolSize {
			poolSize = s.paths.Len()
		}
	}

	if poolSize == 0 {
		poolSize = 1
	}

	return poolSize
}
