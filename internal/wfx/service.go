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
	ErrEmptyDirectory = errors.New("directory is empty")
	ErrInvalidHandle  = errors.New("invalid find handle")
	ErrUserAbort      = errors.New("transfer cancelled by user")
)

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

	configPath string
	config     config.Config
	configInfo fileStamp
	loaded     bool

	handles   map[uint64]*findState
	nextToken uint64
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
	s.config = config.Config{}
	s.configInfo = fileStamp{}
	s.loaded = false
	s.mu.Unlock()
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
	s.config = config.Config{}
	s.configInfo = fileStamp{}
	s.loaded = false
	s.mu.Unlock()
}

func (s *Service) ConfigPath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.configPath
}

func (s *Service) FindFirst(remote string) (uint64, FindData, error) {
	// Total Commander may enter directly through a saved subdirectory instead
	// of enumerating the plugin root first. Always reload at this new listing
	// boundary so that the selected connection reflects the current INI.
	if err := s.reloadConfig(true); err != nil {
		return 0, FindData{}, fmt.Errorf("load %s: %w", s.ConfigPath(), err)
	}

	profileName, relative, err := pathutil.ParseRemote(remote)
	if err != nil {
		return 0, FindData{}, err
	}
	if profileName == "" {
		entries := make([]FindData, 0)
		s.mu.RLock()
		for _, name := range s.config.Names() {
			entries = append(entries, FindData{Name: name, Directory: true})
		}
		s.mu.RUnlock()
		return s.openFind(entries)
	}

	profile, ok := s.profile(profileName)
	if !ok {
		return 0, FindData{}, fmt.Errorf("profile %q is not configured", profileName)
	}
	entries, err := s.backend.List(context.Background(), profile, relative)
	if err != nil {
		return 0, FindData{}, fmt.Errorf("list %s: %w", remote, err)
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
	return s.openFind(data)
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

	object, err := s.backend.Download(context.Background(), profile, key)
	if err != nil {
		return FileNotFound, fmt.Errorf("download %s: %w", remote, err)
	}
	if object.Body == nil {
		return FileReadError, fmt.Errorf("download %s returned an empty body", remote)
	}
	defer object.Body.Close()

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

	progress := newProgress(s.callbacksSnapshot(), remote, local, object.Size)
	if progress.start() {
		return FileUserAbort, ErrUserAbort
	}
	writer := &progressWriter{writer: temp, progress: progress}
	_, copyErr := io.CopyBuffer(writer, object.Body, make([]byte, 1024*1024))
	if closeErr := temp.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		if errors.Is(copyErr, ErrUserAbort) {
			return FileUserAbort, ErrUserAbort
		}
		if writer.writeErr != nil {
			return FileWriteError, writer.writeErr
		}
		return FileReadError, copyErr
	}
	if progress.finish() {
		return FileUserAbort, ErrUserAbort
	}

	if exists {
		if err := os.Remove(local); err != nil {
			return FileWriteError, err
		}
	}
	if err := os.Rename(tempName, local); err != nil {
		return FileWriteError, err
	}
	removeTemp = false

	if flags&CopyMove != 0 {
		if err := s.backend.Delete(context.Background(), profile, key); err != nil {
			return FileReadError, fmt.Errorf("delete after download %s: %w", remote, err)
		}
	}
	return FileOK, nil
}

func (s *Service) PutFile(local, remote string, flags int) (int, error) {
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

	exists, err := s.backend.Head(context.Background(), profile, key)
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
	reader := &progressReader{reader: file, progress: progress}
	uploadErr := s.backend.Upload(context.Background(), profile, key, reader, info.Size())
	if uploadErr != nil {
		if errors.Is(uploadErr, ErrUserAbort) {
			return FileUserAbort, ErrUserAbort
		}
		if reader.readErr != nil {
			return FileReadError, reader.readErr
		}
		return FileWriteError, fmt.Errorf("upload %s: %w", remote, uploadErr)
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
	profile, key, err := s.resolveObject(remote)
	if err != nil {
		return false, err
	}
	if err := s.backend.Delete(context.Background(), profile, key); err != nil {
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
	s.mu.RLock()
	filename := s.configPath
	loaded := s.loaded
	stamp := s.configInfo
	s.mu.RUnlock()
	if filename == "" {
		return errors.New("Total Commander did not provide the settings directory")
	}
	info, err := os.Stat(filename)
	if err != nil {
		return err
	}
	current := fileStamp{modTime: info.ModTime(), size: info.Size()}
	if loaded && !force && current == stamp {
		return nil
	}
	cfg, err := config.Load(filename)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.config = cfg
	s.configInfo = current
	s.loaded = true
	s.mu.Unlock()

	callbacks := s.callbacksSnapshot()
	if callbacks.Log != nil {
		names := cfg.Names()
		profiles := "<none>"
		if len(names) > 0 {
			profiles = strings.Join(names, ", ")
		}
		callbacks.Log(MessageDetails, fmt.Sprintf("loaded %s; profiles: %s", filename, profiles))
	}
	return nil
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

type progressWriter struct {
	writer   io.Writer
	progress *progress
	writeErr error
}

func (w *progressWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	if err != nil {
		w.writeErr = err
		return n, err
	}
	if n > 0 && w.progress.update(w.progress.done+int64(n)) {
		return n, ErrUserAbort
	}
	return n, nil
}

type progressReader struct {
	reader   io.Reader
	progress *progress
	readErr  error
}

func (r *progressReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	if n > 0 && r.progress.update(r.progress.done+int64(n)) {
		return n, ErrUserAbort
	}
	if err != nil && !errors.Is(err, io.EOF) {
		r.readErr = err
	}
	return n, err
}
