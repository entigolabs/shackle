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
)

// Object is one stored object.
type Object struct {
	Key  string
	Size int64
}

// Store is one bucket.
type Store interface {
	// List returns the objects whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Download writes the object to w.
	Download(ctx context.Context, key string, w io.Writer) error
	// Upload stores size bytes from r under key.
	Upload(ctx context.Context, key string, r io.ReadSeeker, size int64) error
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
			return fmt.Errorf("object %q: %w", obj.Key, err)
		}
		o.Log("%s -> %s", obj.Key, local)
		if err := downloadFile(ctx, store, obj.Key, local); err != nil {
			return err
		}
	}
	return nil
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
	return filepath.WalkDir(o.Source, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(o.Source, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if exclude.match(rel) {
			return nil
		}
		o.Log("%s -> %s", p, prefix+rel)
		return uploadFile(ctx, store, p, prefix+rel)
	})
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
