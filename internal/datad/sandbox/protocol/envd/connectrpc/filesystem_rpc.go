/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package envdconnectrpc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/fsnotify/fsnotify"
	"google.golang.org/protobuf/types/known/timestamppb"

	fspb "github.com/nekomeowww/rc/apis/sdk/go/envd/filesystem"
	"github.com/nekomeowww/rc/apis/sdk/go/envd/filesystem/filesystemconnect"
)

type filesystemHandler struct {
	server *Server
}

type managedWatcher struct {
	watcher      *fsnotify.Watcher
	basePath     string
	recursive    bool
	includeEntry bool
	mu           sync.Mutex
	events       []*fspb.FilesystemEvent
	done         chan struct{}
}

func (h *filesystemHandler) Stat(ctx context.Context, req *fspb.StatRequest) (*fspb.StatResponse, error) {
	if err := h.server.validateRPCUser(ctx); err != nil {
		return nil, err
	}
	resolved := h.server.resolvePath(req.GetPath())
	entry, err := h.server.statEntry(resolved)
	if err != nil {
		return nil, mapFSError(err)
	}
	return &fspb.StatResponse{Entry: entry}, nil
}

func (h *filesystemHandler) MakeDir(ctx context.Context, req *fspb.MakeDirRequest) (*fspb.MakeDirResponse, error) {
	if err := h.server.validateRPCUser(ctx); err != nil {
		return nil, err
	}
	resolved := h.server.resolvePath(req.GetPath())
	if info, err := os.Stat(resolved); err == nil {
		if info.IsDir() {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Sprintf("directory %q already exists", resolved))
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("path %q exists and is not a directory", resolved))
	}
	if err := os.MkdirAll(resolved, 0o755); err != nil {
		return nil, mapFSError(err)
	}
	entry, err := h.server.statEntry(resolved)
	if err != nil {
		return nil, mapFSError(err)
	}
	return &fspb.MakeDirResponse{Entry: entry}, nil
}

func (h *filesystemHandler) Move(ctx context.Context, req *fspb.MoveRequest) (*fspb.MoveResponse, error) {
	if err := h.server.validateRPCUser(ctx); err != nil {
		return nil, err
	}
	src := h.server.resolvePath(req.GetSource())
	dst := h.server.resolvePath(req.GetDestination())
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, mapFSError(err)
	}
	if err := os.Rename(src, dst); err != nil {
		return nil, mapFSError(err)
	}
	h.server.moveFileMetadata(src, dst)
	entry, err := h.server.statEntry(dst)
	if err != nil {
		return nil, mapFSError(err)
	}
	return &fspb.MoveResponse{Entry: entry}, nil
}

func (h *filesystemHandler) ListDir(ctx context.Context, req *fspb.ListDirRequest) (*fspb.ListDirResponse, error) {
	if err := h.server.validateRPCUser(ctx); err != nil {
		return nil, err
	}
	root := h.server.resolvePath(req.GetPath())
	info, err := os.Stat(root)
	if err != nil {
		return nil, mapFSError(err)
	}
	if !info.IsDir() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("path %q is not a directory", root))
	}
	maxDepth := int(req.GetDepth())
	if maxDepth <= 0 {
		maxDepth = 1
	}

	entries := make([]*fspb.EntryInfo, 0)
	if err := h.walkDepth(root, 1, maxDepth, &entries); err != nil {
		return nil, mapFSError(err)
	}
	return &fspb.ListDirResponse{Entries: entries}, nil
}

func (h *filesystemHandler) walkDepth(dir string, currentDepth, maxDepth int, out *[]*fspb.EntryInfo) error {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, item := range dirEntries {
		full := filepath.Join(dir, item.Name())
		entry, err := h.server.statEntry(full)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		*out = append(*out, entry)
		if item.IsDir() && currentDepth < maxDepth {
			if err := h.walkDepth(full, currentDepth+1, maxDepth, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *filesystemHandler) Remove(ctx context.Context, req *fspb.RemoveRequest) (*fspb.RemoveResponse, error) {
	if err := h.server.validateRPCUser(ctx); err != nil {
		return nil, err
	}
	resolved := h.server.resolvePath(req.GetPath())
	if _, err := os.Lstat(resolved); err != nil {
		return nil, mapFSError(err)
	}
	if err := os.RemoveAll(resolved); err != nil {
		return nil, mapFSError(err)
	}
	h.server.deleteFileMetadata(resolved)
	return &fspb.RemoveResponse{}, nil
}

func (h *filesystemHandler) WatchDir(ctx context.Context, req *fspb.WatchDirRequest, stream filesystemconnect.FilesystemWatchDirServerStream) error {
	if err := h.server.validateRPCUser(ctx); err != nil {
		return err
	}
	basePath := h.server.resolvePath(req.GetPath())
	watcher, err := openFSWatcher(basePath, req.GetRecursive())
	if err != nil {
		return mapFSError(err)
	}
	defer func() { _ = watcher.Close() }()

	if err := stream.Send(&fspb.WatchDirResponse{
		Event: &fspb.WatchDirResponse_Start{Start: &fspb.WatchDirResponse_StartEvent{}},
	}); err != nil {
		return err
	}

	ticker := time.NewTicker(KeepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := stream.Send(&fspb.WatchDirResponse{
				Event: &fspb.WatchDirResponse_Keepalive{Keepalive: &fspb.WatchDirResponse_KeepAlive{}},
			}); err != nil {
				return err
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			return connect.NewError(connect.CodeInternal, err.Error())
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if req.GetRecursive() && ev.Has(fsnotify.Create) {
				if info, statErr := os.Stat(ev.Name); statErr == nil && info.IsDir() {
					_ = addWatchRecursive(watcher, ev.Name)
				}
			}
			pbEv := h.server.toFilesystemEvent(basePath, ev, req.GetIncludeEntry())
			if pbEv == nil {
				continue
			}
			if err := stream.Send(&fspb.WatchDirResponse{
				Event: &fspb.WatchDirResponse_Filesystem{Filesystem: pbEv},
			}); err != nil {
				return err
			}
		}
	}
}

func (h *filesystemHandler) CreateWatcher(ctx context.Context, req *fspb.CreateWatcherRequest) (*fspb.CreateWatcherResponse, error) {
	if err := h.server.validateRPCUser(ctx); err != nil {
		return nil, err
	}
	basePath := h.server.resolvePath(req.GetPath())
	watcher, err := openFSWatcher(basePath, req.GetRecursive())
	if err != nil {
		return nil, mapFSError(err)
	}

	mw := &managedWatcher{
		watcher:      watcher,
		basePath:     basePath,
		recursive:    req.GetRecursive(),
		includeEntry: req.GetIncludeEntry(),
		done:         make(chan struct{}),
	}

	h.server.watcherMu.Lock()
	h.server.nextWatch++
	id := strconv.FormatUint(h.server.nextWatch, 10)
	h.server.watchers[id] = mw
	h.server.watcherMu.Unlock()

	go h.server.pumpWatcher(mw)
	return &fspb.CreateWatcherResponse{WatcherId: id}, nil
}

func (h *filesystemHandler) GetWatcherEvents(ctx context.Context, req *fspb.GetWatcherEventsRequest) (*fspb.GetWatcherEventsResponse, error) {
	if err := h.server.validateRPCUser(ctx); err != nil {
		return nil, err
	}
	h.server.watcherMu.Lock()
	mw := h.server.watchers[req.GetWatcherId()]
	h.server.watcherMu.Unlock()
	if mw == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Sprintf("watcher %q not found", req.GetWatcherId()))
	}
	mw.mu.Lock()
	events := mw.events
	mw.events = nil
	mw.mu.Unlock()
	return &fspb.GetWatcherEventsResponse{Events: events}, nil
}

func (h *filesystemHandler) RemoveWatcher(ctx context.Context, req *fspb.RemoveWatcherRequest) (*fspb.RemoveWatcherResponse, error) {
	if err := h.server.validateRPCUser(ctx); err != nil {
		return nil, err
	}
	h.server.watcherMu.Lock()
	mw := h.server.watchers[req.GetWatcherId()]
	delete(h.server.watchers, req.GetWatcherId())
	h.server.watcherMu.Unlock()
	if mw == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Sprintf("watcher %q not found", req.GetWatcherId()))
	}
	_ = mw.watcher.Close()
	<-mw.done
	return &fspb.RemoveWatcherResponse{}, nil
}

func (s *Server) pumpWatcher(mw *managedWatcher) {
	defer close(mw.done)
	for {
		select {
		case ev, ok := <-mw.watcher.Events:
			if !ok {
				return
			}
			if mw.recursive && ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = addWatchRecursive(mw.watcher, ev.Name)
				}
			}
			if pbEv := s.toFilesystemEvent(mw.basePath, ev, mw.includeEntry); pbEv != nil {
				mw.mu.Lock()
				mw.events = append(mw.events, pbEv)
				mw.mu.Unlock()
			}
		case _, ok := <-mw.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

func (s *Server) closeAllWatchers() {
	s.watcherMu.Lock()
	list := make([]*managedWatcher, 0, len(s.watchers))
	for id, mw := range s.watchers {
		list = append(list, mw)
		delete(s.watchers, id)
	}
	s.watcherMu.Unlock()
	for _, mw := range list {
		_ = mw.watcher.Close()
		<-mw.done
	}
}

func openFSWatcher(basePath string, recursive bool) (*fsnotify.Watcher, error) {
	info, err := os.Stat(basePath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path %q is not a directory", basePath)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if recursive {
		if err := addWatchRecursive(watcher, basePath); err != nil {
			_ = watcher.Close()
			return nil, err
		}
	} else if err := watcher.Add(basePath); err != nil {
		_ = watcher.Close()
		return nil, err
	}
	return watcher, nil
}

func addWatchRecursive(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return watcher.Add(path)
		}
		return nil
	})
}

func (s *Server) toFilesystemEvent(basePath string, ev fsnotify.Event, includeEntry bool) *fspb.FilesystemEvent {
	var evType fspb.EventType
	switch {
	case ev.Has(fsnotify.Create):
		evType = fspb.EventType_EVENT_TYPE_CREATE
	case ev.Has(fsnotify.Write):
		evType = fspb.EventType_EVENT_TYPE_WRITE
	case ev.Has(fsnotify.Remove):
		evType = fspb.EventType_EVENT_TYPE_REMOVE
	case ev.Has(fsnotify.Rename):
		evType = fspb.EventType_EVENT_TYPE_RENAME
	case ev.Has(fsnotify.Chmod):
		evType = fspb.EventType_EVENT_TYPE_CHMOD
	default:
		return nil
	}
	rel, err := filepath.Rel(basePath, ev.Name)
	if err != nil || rel == "." {
		rel = filepath.Base(ev.Name)
	}
	pbEv := &fspb.FilesystemEvent{
		Name: filepath.ToSlash(rel),
		Type: evType,
	}
	if includeEntry && evType != fspb.EventType_EVENT_TYPE_REMOVE && evType != fspb.EventType_EVENT_TYPE_RENAME {
		if entry, err := s.statEntry(ev.Name); err == nil {
			pbEv.Entry = entry
		}
	}
	return pbEv
}

func (s *Server) statEntry(path string) (*fspb.EntryInfo, error) {
	linfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	isSymlink := linfo.Mode()&os.ModeSymlink != 0
	targetInfo := linfo
	var symlinkTarget *string
	if isSymlink {
		if target, readErr := os.Readlink(path); readErr == nil {
			symlinkTarget = &target
		}
		if statInfo, statErr := os.Stat(path); statErr == nil {
			targetInfo = statInfo
		}
	}

	fileType := fspb.FileType_FILE_TYPE_FILE
	if targetInfo.IsDir() {
		fileType = fspb.FileType_FILE_TYPE_DIRECTORY
	} else if isSymlink && targetInfo == linfo {
		fileType = fspb.FileType_FILE_TYPE_SYMLINK
	}

	return &fspb.EntryInfo{
		Name:          linfo.Name(),
		Type:          fileType,
		Path:          path,
		Size:          targetInfo.Size(),
		Mode:          uint32(targetInfo.Mode().Perm()),
		Permissions:   targetInfo.Mode().String(),
		Owner:         s.runtimeUser,
		Group:         s.runtimeUser,
		ModifiedTime:  timestamppb.New(targetInfo.ModTime()),
		SymlinkTarget: symlinkTarget,
		Metadata:      s.getFileMetadata(path),
		IsSymlink:     isSymlink,
	}, nil
}

func mapFSError(err error) error {
	if err == nil {
		return nil
	}
	if connectErr, ok := errors.AsType[*connect.Error](err); ok {
		return connectErr
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return connect.NewError(connect.CodeNotFound, err.Error())
	case errors.Is(err, os.ErrPermission):
		return connect.NewError(connect.CodePermissionDenied, err.Error())
	case errors.Is(err, os.ErrExist):
		return connect.NewError(connect.CodeAlreadyExists, err.Error())
	case errors.Is(err, syscall.ENOSPC):
		return connect.NewError(connect.CodeResourceExhausted, err.Error())
	case strings.Contains(err.Error(), "is not a directory"):
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	default:
		return connect.NewError(connect.CodeInternal, err.Error())
	}
}
