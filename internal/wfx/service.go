package wfx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example/wfxs3/internal/config"
	pathutil "github.com/example/wfxs3/internal/path"
	"github.com/example/wfxs3/internal/s3store"
)

const (
	FileOK                  = 0
	FileExists              = 1
	FileNotFound            = 2
	FileReadError           = 3
	FileWriteError          = 4
	FileUserAbort           = 5
	FileNotSupported        = 6
	FileExistsResumeAllowed = 7

	CopyOverwrite         = 1
	CopyResume            = 2
	CopyMove              = 4
	RequestMessageOK      = 8
	MessageDetails        = 3
	MessageImportantError = 6
)

var (
	ErrEmptyDirectory  = errors.New("directory is empty")
	ErrInvalidHandle   = errors.New("invalid find handle")
	ErrUserAbort       = errors.New("transfer cancelled by user")
	ErrTransferStalled = errors.New("transfer stalled")
)

// metadataTimeout bounds listing, existence, and delete calls. Total Commander
// runs these on its user-interface thread, so an endpoint that accepts a
// connection and then goes silent must not block the file manager forever.
// It is a variable so that tests can shorten it.
var metadataTimeout = 60 * time.Second

// Transfers have no overall deadline because a large file may legitimately
// take hours. The transport's response-header timeout only covers the wait for
// a response, not a body that stops moving, so runTransfer fails a transfer
// once no bytes have moved for transferIdleTimeout. It is longer than the
// response-header timeout, which leaves a server that is slow to answer after
// an upload to that timeout and the SDK's retries. progressInterval is how
// often runTransfer samples a transfer and offers Total Commander a progress
// update. Both are variables so that tests can shorten them.
var (
	transferIdleTimeout = 60 * time.Second
	progressInterval    = 100 * time.Millisecond
)

// metadataContext bounds a short control-plane call.
func metadataContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), metadataTimeout)
}

// Callbacks contains the already-adapted Total Commander callbacks. The
// service layer remains independent from cgo and is therefore straightforward
// to test.
type Callbacks struct {
	Progress func(source, target string, percent int) bool
	Log      func(messageType int, message string)
	Request  func(requestType int, title, text, defaultText string) (string, bool)
}

// FindData is the platform-neutral subset of WIN32_FIND_DATAW that the cgo
// layer needs to render.
type FindData struct {
	Name         string
	Directory    bool
	Size         int64
	LastModified time.Time
}

type findState struct {
	entries []FindData
	next    int
}

// Service implements the WFX behavior without exposing C types.
type Service struct {
	mu sync.RWMutex

	callbacks Callbacks
	backend   s3store.Backend

	configPath     string
	config         config.Config
	configInfo     fileStamp
	loaded         bool
	configErr      error     // why the last load failed; nil once one succeeds
	configLoadedAt time.Time // when config was last parsed

	handles   map[uint64]*findState
	nextToken uint64

	test connectionTest

	activity activityLog
}

type fileStamp struct {
	modTime time.Time
	size    int64
}

func New(backend s3store.Backend) *Service {
	return &Service{
		backend:   backend,
		handles:   make(map[uint64]*findState),
		nextToken: 1,
	}
}

func (s *Service) SetCallbacks(callbacks Callbacks) {
	s.mu.Lock()
	s.callbacks = callbacks
	s.mu.Unlock()
}

// ResetConfig discards the parsed configuration and its file stamp. The WFX
// host can keep the DLL loaded between sessions, so initialization must not
// rely on a fresh Service value to clear configuration state.
func (s *Service) ResetConfig() {
	s.mu.Lock()
	s.clearConfigLocked()
	s.mu.Unlock()
}

// clearConfigLocked discards the parsed configuration. s.mu must be held.
func (s *Service) clearConfigLocked() {
	s.config = config.Config{}
	s.configInfo = fileStamp{}
	s.loaded = false
	s.configErr = nil
	s.configLoadedAt = time.Time{}
}

// SetDefaultIniName converts Total Commander's suggested wincmd.ini path into
// the plugin-specific settings path recommended by the WFX SDK.
func (s *Service) SetDefaultIniName(defaultIniName string) {
	defaultIniName = strings.TrimSpace(defaultIniName)
	if defaultIniName == "" {
		return
	}
	s.mu.Lock()
	s.configPath = filepath.Join(filepath.Dir(defaultIniName), "wfxs3.ini")
	s.clearConfigLocked()
	s.mu.Unlock()
}

func (s *Service) ConfigPath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.configPath
}

func (s *Service) FindFirst(remote string) (uint64, FindData, error) {
	start := time.Now()
	entries, err := s.list(remote)
	if err != nil {
		s.record("list", remote, "", start, "error", err)
		return 0, FindData{}, err
	}
	s.record("list", remote, "("+countNoun(len(entries), "entry", "entries")+")", start, "ok", nil)
	return s.openFind(entries)
}

// list returns the sorted entries of a remote directory.
func (s *Service) list(remote string) ([]FindData, error) {
	// Total Commander may enter directly through a saved subdirectory instead
	// of enumerating the plugin root first. Always reload at this new listing
	// boundary so that the selected connection reflects the current INI.
	if err := s.reloadConfig(true); err != nil {
		return nil, fmt.Errorf("load %s: %w", s.ConfigPath(), err)
	}

	profileName, relative, err := pathutil.ParseRemote(remote)
	if err != nil {
		return nil, err
	}
	if profileName == "" {
		entries := make([]FindData, 0)
		s.mu.RLock()
		for _, name := range s.config.Names() {
			entries = append(entries, FindData{Name: name, Directory: true})
		}
		s.mu.RUnlock()
		return entries, nil
	}

	profile, ok := s.profile(profileName)
	if !ok {
		return nil, fmt.Errorf("profile %q is not configured", profileName)
	}
	listCtx, cancelList := metadataContext()
	defer cancelList()
	entries, err := s.backend.List(listCtx, profile, relative)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", remote, err)
	}
	data := make([]FindData, 0, len(entries))
	for _, entry := range entries {
		data = append(data, FindData{
			Name:         entry.Name,
			Directory:    entry.Directory,
			Size:         entry.Size,
			LastModified: entry.LastModified,
		})
	}
	sort.SliceStable(data, func(i, j int) bool {
		if data[i].Directory != data[j].Directory {
			return data[i].Directory
		}
		return strings.ToLower(data[i].Name) < strings.ToLower(data[j].Name)
	})
	return data, nil
}

func (s *Service) openFind(entries []FindData) (uint64, FindData, error) {
	if len(entries) == 0 {
		return 0, FindData{}, ErrEmptyDirectory
	}
	s.mu.Lock()
	token := s.nextToken
	s.nextToken++
	if token == 0 {
		token = s.nextToken
		s.nextToken++
	}
	s.handles[token] = &findState{entries: entries, next: 1}
	s.mu.Unlock()
	return token, entries[0], nil
}

func (s *Service) FindNext(token uint64) (FindData, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.handles[token]
	if !ok {
		return FindData{}, false, ErrInvalidHandle
	}
	if state.next >= len(state.entries) {
		return FindData{}, false, nil
	}
	entry := state.entries[state.next]
	state.next++
	return entry, true, nil
}

func (s *Service) FindClose(token uint64) {
	s.mu.Lock()
	delete(s.handles, token)
	s.mu.Unlock()
}

func (s *Service) GetFile(remote, local string, flags int) (int, error) {
	start := time.Now()
	status, err := s.getFile(remote, local, flags)
	s.recordTransfer("get", remote, local, flags, start, status, err)
	return status, err
}

func (s *Service) getFile(remote, local string, flags int) (int, error) {
	if flags&CopyResume != 0 {
		return FileNotSupported, nil
	}
	profile, key, err := s.resolveObject(remote)
	if err != nil {
		return FileNotFound, err
	}

	exists, localIsDir, err := localState(local)
	if err != nil {
		return FileWriteError, err
	}
	if localIsDir {
		return FileWriteError, fmt.Errorf("local path is a directory: %s", local)
	}
	if exists && flags&CopyOverwrite == 0 {
		return FileExists, nil
	}

	temp, err := os.CreateTemp(filepath.Dir(local), ".wfxs3-download-*")
	if err != nil {
		return FileWriteError, err
	}
	tempName := temp.Name()
	removeTemp := true
	defer func() {
		_ = temp.Close()
		if removeTemp {
			_ = os.Remove(tempName)
		}
	}()

	progress := newProgress(s.callbacksSnapshot(), remote, local, 0)
	if progress.start() {
		return FileUserAbort, ErrUserAbort
	}
	var done, total atomic.Int64
	writer := &countingWriter{writer: temp, done: &done}
	err = runTransfer(progress, &done, &total, func(ctx context.Context) error {
		object, err := s.backend.Download(ctx, profile, key)
		if err != nil {
			return err
		}
		if object.Body == nil {
			return errors.New("the response has no body")
		}
		defer object.Body.Close()
		total.Store(object.Size)
		_, err = io.CopyBuffer(writer, object.Body, make([]byte, 1024*1024))
		return err
	})
	switch {
	case errors.Is(err, ErrUserAbort):
		return FileUserAbort, ErrUserAbort
	case writer.err != nil:
		return FileWriteError, writer.err
	case errors.Is(err, os.ErrNotExist):
		return FileNotFound, fmt.Errorf("download %s: %w", remote, err)
	case err != nil:
		return FileReadError, fmt.Errorf("download %s: %w", remote, err)
	}
	if err := temp.Close(); err != nil {
		return FileWriteError, err
	}
	progress.total = total.Load()
	if progress.finish() {
		return FileUserAbort, ErrUserAbort
	}

	if err := replaceFile(tempName, local); err != nil {
		return FileWriteError, err
	}
	removeTemp = false

	if flags&CopyMove != 0 {
		deleteCtx, cancelDelete := metadataContext()
		defer cancelDelete()
		if err := s.backend.Delete(deleteCtx, profile, key); err != nil {
			return FileReadError, fmt.Errorf("delete after download %s: %w", remote, err)
		}
	}
	return FileOK, nil
}

func (s *Service) PutFile(local, remote string, flags int) (int, error) {
	start := time.Now()
	status, err := s.putFile(local, remote, flags)
	s.recordTransfer("put", local, remote, flags, start, status, err)
	return status, err
}

func (s *Service) putFile(local, remote string, flags int) (int, error) {
	if flags&CopyResume != 0 {
		return FileNotSupported, nil
	}
	profile, key, err := s.resolveObject(remote)
	if err != nil {
		return FileNotFound, err
	}
	info, err := os.Stat(local)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileNotFound, err
		}
		return FileReadError, err
	}
	if info.IsDir() {
		return FileReadError, fmt.Errorf("local path is a directory: %s", local)
	}

	headCtx, cancelHead := metadataContext()
	defer cancelHead()
	exists, err := s.backend.Head(headCtx, profile, key)
	if err != nil {
		return FileWriteError, fmt.Errorf("check remote %s: %w", remote, err)
	}
	if exists && flags&CopyOverwrite == 0 {
		return FileExists, nil
	}

	file, err := os.Open(local)
	if err != nil {
		return FileReadError, err
	}
	defer file.Close()

	progress := newProgress(s.callbacksSnapshot(), local, remote, info.Size())
	if progress.start() {
		return FileUserAbort, ErrUserAbort
	}
	var done, total atomic.Int64
	total.Store(info.Size())
	reader := &countingReader{reader: file, done: &done}
	err = runTransfer(progress, &done, &total, func(ctx context.Context) error {
		return s.backend.Upload(ctx, profile, key, reader, info.Size())
	})
	if err != nil {
		if errors.Is(err, ErrUserAbort) {
			return FileUserAbort, ErrUserAbort
		}
		if readErr := reader.readError(); readErr != nil {
			return FileReadError, readErr
		}
		return FileWriteError, fmt.Errorf("upload %s: %w", remote, err)
	}
	if progress.finish() {
		return FileUserAbort, ErrUserAbort
	}
	if flags&CopyMove != 0 {
		if err := os.Remove(local); err != nil {
			return FileWriteError, fmt.Errorf("remove local source after upload: %w", err)
		}
	}
	return FileOK, nil
}

func (s *Service) DeleteFile(remote string) (bool, error) {
	start := time.Now()
	ok, err := s.deleteFile(remote)
	s.record("delete", remote, "", start, resultStatus(err), err)
	return ok, err
}

func (s *Service) deleteFile(remote string) (bool, error) {
	profile, key, err := s.resolveObject(remote)
	if err != nil {
		return false, err
	}
	deleteCtx, cancelDelete := metadataContext()
	defer cancelDelete()
	if err := s.backend.Delete(deleteCtx, profile, key); err != nil {
		return false, fmt.Errorf("delete %s: %w", remote, err)
	}
	return true, nil
}

func (s *Service) resolveObject(remote string) (config.Profile, string, error) {
	if err := s.reloadConfig(false); err != nil {
		return config.Profile{}, "", fmt.Errorf("load %s: %w", s.ConfigPath(), err)
	}
	profileName, relative, err := pathutil.ParseRemote(remote)
	if err != nil {
		return config.Profile{}, "", err
	}
	if profileName == "" || relative == "" {
		return config.Profile{}, "", fmt.Errorf("%q is a directory, not an object", remote)
	}
	profile, ok := s.profile(profileName)
	if !ok {
		return config.Profile{}, "", fmt.Errorf("profile %q is not configured", profileName)
	}
	return profile, relative, nil
}

func (s *Service) profile(name string) (config.Profile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	profile, ok := s.config.Profile(name)
	return profile, ok
}

func (s *Service) reloadConfig(force bool) error {
	start := time.Now()
	s.mu.RLock()
	filename := s.configPath
	loaded := s.loaded
	stamp := s.configInfo
	s.mu.RUnlock()
	if filename == "" {
		return s.configFailed(errors.New("Total Commander did not provide the settings directory"))
	}
	info, err := os.Stat(filename)
	if err != nil {
		return s.configFailed(err)
	}
	current := fileStamp{modTime: info.ModTime(), size: info.Size()}
	if loaded && !force && current == stamp {
		return nil
	}
	cfg, err := config.Load(filename)
	if err != nil {
		return s.configFailed(err)
	}
	s.mu.Lock()
	changed := !s.loaded || s.configErr != nil || current != s.configInfo
	s.config = cfg
	s.configInfo = current
	s.loaded = true
	s.configErr = nil
	s.configLoadedAt = time.Now()
	s.mu.Unlock()

	names := cfg.Names()
	profiles := "<none>"
	if len(names) > 0 {
		profiles = strings.Join(names, ", ")
	}
	// FindFirst reloads on every listing, so record only a changed file.
	if changed {
		s.record("config", filename, "(profiles: "+profiles+")", start, "ok", nil)
	}
	callbacks := s.callbacksSnapshot()
	if callbacks.Log != nil {
		callbacks.Log(MessageDetails, fmt.Sprintf("loaded %s; profiles: %s", filename, profiles))
	}
	return nil
}

// configFailed keeps a load failure for the debug report and returns it.
func (s *Service) configFailed(err error) error {
	s.mu.Lock()
	s.configErr = err
	s.mu.Unlock()
	return err
}

func (s *Service) callbacksSnapshot() Callbacks {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.callbacks
}

func (s *Service) ReportError(err error) {
	if err == nil {
		return
	}
	callbacks := s.callbacksSnapshot()
	message := err.Error()
	if callbacks.Log != nil {
		callbacks.Log(MessageImportantError, message)
	}
	if callbacks.Request != nil {
		_, _ = callbacks.Request(RequestMessageOK, "WFX S3", message, "")
	}
}

func localState(filename string) (exists, isDir bool, err error) {
	info, err := os.Stat(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, info.IsDir(), nil
}

// replaceFile moves source over target. os.Rename replaces an existing target
// in a single MoveFileEx call, so a failed rename leaves the old file intact.
// MoveFileEx refuses to replace a read-only file; the caller has already
// confirmed the overwrite, so clear the attribute and try once more.
func replaceFile(source, target string) error {
	err := os.Rename(source, target)
	if err == nil {
		return nil
	}
	info, statErr := os.Stat(target)
	if statErr != nil || info.Mode().Perm()&0o200 != 0 {
		return err
	}
	if os.Chmod(target, 0o666) != nil {
		return err
	}
	return os.Rename(source, target)
}

// runTransfer runs work on its own goroutine while the calling goroutine keeps
// reporting progress. Called from a plugin export, the calling goroutine owns
// Total Commander's thread, the only thread that may invoke its callbacks, so
// polling here keeps Cancel responsive even while the network is stalled.
// Cancel and stalls cancel work's context, and the AWS SDK never retries a
// canceled request. runTransfer always waits for work to return, so callers
// may then inspect anything work wrote.
func runTransfer(p *progress, done, total *atomic.Int64, work func(context.Context) error) error {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	result := make(chan error, 1)
	go func() { result <- work(ctx) }()
	stop := func(cause error) error {
		cancel(cause)
		<-result
		return cause
	}

	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	lastDone, lastMoved := done.Load(), time.Now()
	for {
		select {
		case err := <-result:
			return err
		case now := <-ticker.C:
			current := done.Load()
			if current != lastDone {
				lastDone, lastMoved = current, now
			}
			p.total = total.Load()
			if p.update(current) {
				return stop(ErrUserAbort)
			}
			if idle := now.Sub(lastMoved); idle >= transferIdleTimeout {
				return stop(fmt.Errorf("no data transferred for %s: %w", idle.Round(time.Second), ErrTransferStalled))
			}
		}
	}
}

type progress struct {
	callbacks Callbacks
	source    string
	target    string
	total     int64
	done      int64
	last      time.Time
	lastPct   int
}

func newProgress(callbacks Callbacks, source, target string, total int64) *progress {
	return &progress{callbacks: callbacks, source: source, target: target, total: total, lastPct: -1}
}

func (p *progress) start() bool {
	return p.report(0, true)
}

func (p *progress) update(done int64) bool {
	p.done = done
	return p.report(done, false)
}

func (p *progress) finish() bool {
	p.done = p.total
	return p.report(p.total, true)
}

func (p *progress) report(done int64, force bool) bool {
	if p.callbacks.Progress == nil {
		return false
	}
	pct := 0
	if p.total > 0 {
		pct = int(done * 100 / p.total)
		if pct > 100 {
			pct = 100
		}
	} else if force {
		// An empty file still needs a definite completion notification.
		pct = 100
	}
	now := time.Now()
	if !force && pct == p.lastPct && now.Sub(p.last) < 250*time.Millisecond {
		return false
	}
	p.last = now
	p.lastPct = pct
	return p.callbacks.Progress(p.source, p.target, pct)
}

// countingWriter receives a download and counts the bytes written locally for
// runTransfer. It is only used by the transfer goroutine, and runTransfer's
// wait makes err safe to read once it returns.
type countingWriter struct {
	writer io.Writer
	done   *atomic.Int64
	err    error
}

func (w *countingWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.done.Add(int64(n))
	if err != nil {
		w.err = err
	}
	return n, err
}

// countingReader feeds an upload and counts the bytes the SDK has read for
// runTransfer. The HTTP transport reads it on its own goroutine, so readErr is
// guarded.
type countingReader struct {
	reader io.ReadSeeker
	done   *atomic.Int64

	mu      sync.Mutex
	readErr error
}

func (r *countingReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	r.done.Add(int64(n))
	if err != nil && !errors.Is(err, io.EOF) {
		r.mu.Lock()
		r.readErr = err
		r.mu.Unlock()
	}
	return n, err
}

// Seek keeps the upload body rewindable. The AWS SDK replays the body to
// compute the SigV4 payload hash for plain-HTTP endpoints, and again before
// every retry; a reader that only satisfies io.Reader fails both with
// "request stream is not seekable". Storing the new position keeps a replayed
// body from being counted as additional transferred bytes.
func (r *countingReader) Seek(offset int64, whence int) (int64, error) {
	position, err := r.reader.Seek(offset, whence)
	if err != nil {
		return position, err
	}
	r.mu.Lock()
	r.readErr = nil
	r.mu.Unlock()
	r.done.Store(position)
	return position, nil
}

// readError returns the last local read failure, if any.
func (r *countingReader) readError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readErr
}
