// Package storage copies files between the local disk and a cloud's object
// storage.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Object is one stored object.
type Object struct {
	Key  string
	Size int64
	// ModTime is when the object was last written; zero when unknown.
	ModTime time.Time
}

// Store is one bucket.
type Store interface {
	// List returns the objects whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Download writes the object to w.
	Download(ctx context.Context, key string, w io.Writer) error
	// Upload stores size bytes from r under key.
	Upload(ctx context.Context, key string, r io.ReadSeeker, size int64) error
	// Delete removes the object.
	Delete(ctx context.Context, key string) error
}

// Location is a parsed object storage URL.
//
//	s3://bucket/key
//	gs://bucket/key
//	az://account/container/key
//	oci://bucket@namespace/key
type Location struct {
	Scheme  string
	Account string // Azure storage account, Oracle namespace
	Bucket  string // bucket or container
	Key     string
}

// Parse reads an object storage URL. ok is false for a local path.
func Parse(s string) (loc Location, ok bool, err error) {
	scheme, rest, found := strings.Cut(s, "://")
	if !found {
		return Location{}, false, nil
	}
	host, key, _ := strings.Cut(rest, "/")
	loc = Location{Scheme: scheme, Bucket: host, Key: key}
	switch scheme {
	case "s3", "gs":
	case "az":
		loc.Account = host
		loc.Bucket, loc.Key, _ = strings.Cut(key, "/")
	case "oci":
		loc.Bucket, loc.Account, found = strings.Cut(host, "@")
		if !found || loc.Account == "" {
			return Location{}, true, fmt.Errorf("%s: an Oracle location is oci://bucket@namespace/key", s)
		}
	default:
		return Location{}, true, fmt.Errorf("%s: unknown scheme %q (s3, gs, az and oci are supported)", s, scheme)
	}
	if loc.Bucket == "" {
		return Location{}, true, fmt.Errorf("%s: no bucket", s)
	}
	return loc, true, nil
}

// CopyOptions is one copy.
type CopyOptions struct {
	Source      string
	Destination string
	// Recursive copies everything under a directory or a key prefix.
	Recursive bool
	// Exclude are patterns of relative paths to leave out. * matches any run
	// of characters, including /, and ? matches one character.
	Exclude []string
	// Log is told about every file copied; nil is silence.
	Log func(format string, args ...any)
}

// Copy copies between a local path and an object storage location, in either
// direction. open returns the store of a location.
func Copy(ctx context.Context, o CopyOptions, open func(context.Context, Location) (Store, error)) error {
	src, srcRemote, err := Parse(o.Source)
	if err != nil {
		return err
	}
	dst, dstRemote, err := Parse(o.Destination)
	if err != nil {
		return err
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	exclude, err := compileExcludes(o.Exclude)
	if err != nil {
		return err
	}
	switch {
	case srcRemote && !dstRemote:
		store, err := open(ctx, src)
		if err != nil {
			return err
		}
		return download(ctx, store, src.Key, o, exclude)
	case !srcRemote && dstRemote:
		store, err := open(ctx, dst)
		if err != nil {
			return err
		}
		return upload(ctx, store, dst.Key, o, exclude)
	case srcRemote && dstRemote:
		return errors.New("copying from one object storage location to another is not supported")
	default:
		return errors.New("one of source and destination must be an object storage location (s3://, gs://, az:// or oci://)")
	}
}

func download(ctx context.Context, store Store, key string, o CopyOptions, exclude excludes) error {
	if !o.Recursive {
		if key == "" || strings.HasSuffix(key, "/") {
			return fmt.Errorf("%s is a prefix, not an object: use --recursive", o.Source)
		}
		dest := o.Destination
		if strings.HasSuffix(dest, string(os.PathSeparator)) || isDir(dest) {
			dest = filepath.Join(dest, path.Base(key))
		}
		o.Log("%s -> %s", o.Source, dest)
		return downloadFile(ctx, store, key, dest)
	}
	// A prefix is a directory: "steps/a" must not match "steps/ab/file".
	prefix := key
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return err
	}
	files := newPool(ctx)
	for _, obj := range objects {
		rel := strings.TrimPrefix(obj.Key, prefix)
		if rel == "" || strings.HasSuffix(rel, "/") {
			continue // a directory marker
		}
		if exclude.match(rel) {
			continue
		}
		local, err := safeJoin(o.Destination, rel)
		if err != nil {
			files.wait()
			return fmt.Errorf("object %q: %w", obj.Key, err)
		}
		o.Log("%s -> %s", obj.Key, local)
		files.do(func(ctx context.Context) error {
			if err := downloadFile(ctx, store, obj.Key, local); err != nil {
				return err
			}
			// The file gets the object's time, so that a later sync can tell
			// what changed since the download.
			if !obj.ModTime.IsZero() {
				_ = os.Chtimes(local, obj.ModTime, obj.ModTime)
			}
			return nil
		})
	}
	return files.wait()
}

func downloadFile(ctx context.Context, store Store, key, dest string) (err error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dest) // no half-written file left behind
		}
	}()
	if err := store.Download(ctx, key, f); err != nil {
		return fmt.Errorf("download %s: %w", key, err)
	}
	return nil
}

func upload(ctx context.Context, store Store, key string, o CopyOptions, exclude excludes) error {
	info, err := os.Stat(o.Source)
	if err != nil {
		return err
	}
	if !o.Recursive {
		if info.IsDir() {
			return fmt.Errorf("%s is a directory: use --recursive", o.Source)
		}
		if key == "" || strings.HasSuffix(key, "/") {
			key += filepath.Base(o.Source)
		}
		o.Log("%s -> %s", o.Source, key)
		return uploadFile(ctx, store, o.Source, key)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", o.Source)
	}
	prefix := key
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	files := newPool(ctx)
	err = walkFiles(o.Source, func(p, rel string, _ os.FileInfo) error {
		if exclude.match(rel) {
			return nil
		}
		o.Log("%s -> %s", p, prefix+rel)
		files.do(func(ctx context.Context) error { return uploadFile(ctx, store, p, prefix+rel) })
		return nil
	})
	if werr := files.wait(); err == nil {
		err = werr
	}
	return err
}

// walkFiles calls fn for every file under root, with its path relative to
// root in slashes. Symbolic links are followed, as the cloud CLIs do: a
// provider cache is a directory of links.
func walkFiles(root string, fn func(path, rel string, info os.FileInfo) error) error {
	return walkDir(root, "", 0, fn)
}

// maxDepth stops a loop of symbolic links.
const maxDepth = 64

func walkDir(dir, rel string, depth int, fn func(path, rel string, info os.FileInfo) error) error {
	if depth > maxDepth {
		return fmt.Errorf("%s: directories nested deeper than %d, a loop of symbolic links?", dir, maxDepth)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		r := e.Name()
		if rel != "" {
			r = rel + "/" + e.Name()
		}
		info, err := os.Stat(p) // follows a link
		if err != nil {
			if os.IsNotExist(err) {
				continue // a dangling link
			}
			return err
		}
		switch {
		case info.IsDir():
			if err := walkDir(p, r, depth+1, fn); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := fn(p, r, info); err != nil {
				return err
			}
		}
	}
	return nil
}

// SyncOptions is one sync.
type SyncOptions struct {
	// Source is a local directory, Destination an object storage prefix.
	Source      string
	Destination string
	// Delete removes objects under the prefix that the directory lacks.
	Delete bool
	// Exclude are patterns of relative paths that are neither uploaded nor
	// deleted.
	Exclude []string
	Log     func(format string, args ...any)
}

// Sync makes an object storage prefix hold what a local directory holds. A
// file is uploaded when the prefix lacks it, when the sizes differ, or when
// the file is newer than the object; everything else is left alone.
func Sync(ctx context.Context, o SyncOptions, open func(context.Context, Location) (Store, error)) error {
	if _, remote, _ := Parse(o.Source); remote {
		return errors.New("sync goes from a local directory to object storage; use cp --recursive to download")
	}
	dst, remote, err := Parse(o.Destination)
	if err != nil {
		return err
	}
	if !remote {
		return errors.New("the destination of sync must be an object storage location (s3://, gs://, az:// or oci://)")
	}
	if info, err := os.Stat(o.Source); err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", o.Source)
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	exclude, err := compileExcludes(o.Exclude)
	if err != nil {
		return err
	}
	store, err := open(ctx, dst)
	if err != nil {
		return err
	}
	prefix := dst.Key
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return err
	}
	remoteObjects := make(map[string]Object, len(objects))
	for _, obj := range objects {
		remoteObjects[strings.TrimPrefix(obj.Key, prefix)] = obj
	}
	seen := map[string]bool{}
	files := newPool(ctx)
	err = walkFiles(o.Source, func(p, rel string, info os.FileInfo) error {
		if exclude.match(rel) {
			return nil
		}
		seen[rel] = true
		if obj, ok := remoteObjects[rel]; ok && obj.Size == info.Size() && !obj.ModTime.IsZero() && !info.ModTime().After(obj.ModTime) {
			return nil
		}
		o.Log("%s -> %s", p, prefix+rel)
		files.do(func(ctx context.Context) error { return uploadFile(ctx, store, p, prefix+rel) })
		return nil
	})
	if werr := files.wait(); err == nil {
		err = werr
	}
	// Nothing is deleted after a failed upload: the prefix keeps what it had.
	if err != nil || !o.Delete {
		return err
	}
	files = newPool(ctx)
	for rel, obj := range remoteObjects {
		if rel == "" || seen[rel] || exclude.match(rel) {
			continue
		}
		o.Log("delete %s", obj.Key)
		files.do(func(ctx context.Context) error {
			if err := store.Delete(ctx, obj.Key); err != nil {
				return fmt.Errorf("delete %s: %w", obj.Key, err)
			}
			return nil
		})
	}
	return files.wait()
}

// Parallel is how many files are transferred at once.
const Parallel = 16

// pool runs tasks, Parallel at a time. After the first failure the tasks
// still waiting are dropped and those running are cancelled.
type pool struct {
	ctx    context.Context
	cancel context.CancelFunc
	slots  chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
	err    error
}

func newPool(ctx context.Context) *pool {
	ctx, cancel := context.WithCancel(ctx)
	return &pool{ctx: ctx, cancel: cancel, slots: make(chan struct{}, Parallel)}
}

// do runs the task when a slot is free; it waits for one.
func (p *pool) do(task func(context.Context) error) {
	select {
	case p.slots <- struct{}{}:
	case <-p.ctx.Done():
		return
	}
	p.wg.Add(1)
	go func() {
		defer func() { <-p.slots; p.wg.Done() }()
		if err := task(p.ctx); err != nil {
			p.once.Do(func() { p.err = err; p.cancel() })
		}
	}()
}

// wait returns the first failure once every task has ended. A cancelled
// parent context is a failure too.
func (p *pool) wait() error {
	p.wg.Wait()
	defer p.cancel()
	if p.err != nil {
		return p.err
	}
	return p.ctx.Err()
}

func uploadFile(ctx context.Context, store Store, file, key string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := store.Upload(ctx, key, f, info.Size()); err != nil {
		return fmt.Errorf("upload %s: %w", key, err)
	}
	return nil
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// safeJoin joins a relative object key to a local directory and refuses a
// key that would land outside it: object keys are chosen by whoever wrote
// the bucket.
func safeJoin(dir, rel string) (string, error) {
	if strings.HasPrefix(rel, "/") {
		return "", errors.New("key is an absolute path")
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return "", errors.New("key leaves the destination directory")
		}
	}
	return filepath.Join(dir, filepath.FromSlash(rel)), nil
}

type excludes []*regexp.Regexp

func (e excludes) match(rel string) bool {
	for _, re := range e {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}

func compileExcludes(patterns []string) (excludes, error) {
	var out excludes
	for _, p := range patterns {
		var b strings.Builder
		b.WriteString("^")
		for _, r := range p {
			switch r {
			case '*':
				b.WriteString(".*")
			case '?':
				b.WriteString(".")
			default:
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		b.WriteString("$")
		re, err := regexp.Compile(b.String())
		if err != nil {
			return nil, fmt.Errorf("exclude pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}
