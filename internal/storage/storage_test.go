package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in     string
		want   Location
		remote bool
		fail   bool
	}{
		{in: "/tmp/dir", remote: false},
		{in: "relative/file", remote: false},
		{in: "s3://bucket/a/b", want: Location{Scheme: "s3", Bucket: "bucket", Key: "a/b"}, remote: true},
		{in: "s3://bucket", want: Location{Scheme: "s3", Bucket: "bucket"}, remote: true},
		{in: "gs://bucket/a/", want: Location{Scheme: "gs", Bucket: "bucket", Key: "a/"}, remote: true},
		{in: "az://account/container/a/b", want: Location{Scheme: "az", Account: "account", Bucket: "container", Key: "a/b"}, remote: true},
		{in: "oci://bucket@ns/a", want: Location{Scheme: "oci", Account: "ns", Bucket: "bucket", Key: "a"}, remote: true},
		{in: "az://account", remote: true, fail: true},
		{in: "oci://bucket/a", remote: true, fail: true},
		{in: "ftp://host/a", remote: true, fail: true},
		{in: "s3:///a", remote: true, fail: true},
	}
	for _, tt := range tests {
		got, remote, err := Parse(tt.in)
		if remote != tt.remote || (err != nil) != tt.fail {
			t.Errorf("Parse(%q): remote %v err %v, want remote %v fail %v", tt.in, remote, err, tt.remote, tt.fail)
			continue
		}
		if err == nil && got != tt.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

// fake is a bucket in memory.
type fake struct {
	objects  map[string]string
	modTimes map[string]time.Time
	failOn   string
	uploads  []string
	deletes  []string
}

func (f *fake) Delete(_ context.Context, key string) error {
	delete(f.objects, key)
	f.deletes = append(f.deletes, key)
	return nil
}

func (f *fake) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	for k, v := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Object{Key: k, Size: int64(len(v)), ModTime: f.modTimes[k]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fake) Download(_ context.Context, key string, w io.Writer) error {
	v, ok := f.objects[key]
	if !ok {
		return errors.New("no such key")
	}
	if key == f.failOn {
		_, _ = io.WriteString(w, v[:1])
		return errors.New("connection reset")
	}
	_, err := io.WriteString(w, v)
	return err
}

func (f *fake) Upload(_ context.Context, key string, r io.ReadSeeker, size int64) error {
	var b bytes.Buffer
	n, err := io.Copy(&b, r)
	if err != nil {
		return err
	}
	if n != size {
		return errors.New("size does not match the content")
	}
	f.objects[key] = b.String()
	f.uploads = append(f.uploads, key)
	return nil
}

func opener(f *fake) func(context.Context, Location) (Store, error) {
	return func(context.Context, Location) (Store, error) { return f, nil }
}

func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestDownloadRecursive(t *testing.T) {
	f := &fake{objects: map[string]string{
		"steps/a/main.tf":                "main",
		"steps/a/sub/values.yaml":        "values",
		"steps/a/.terraform/providers/x": "big",
		"steps/a/":                       "", // a directory marker
		"steps/ab/other":                 "not under steps/a",
		"unrelated":                      "no",
	}}
	dir := t.TempDir()
	err := Copy(context.Background(), CopyOptions{Source: "s3://b/steps/a", Destination: dir, Recursive: true, Exclude: []string{".terraform/*"}}, opener(f))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"main.tf": "main", "sub/values.yaml": "values"}
	if got := tree(t, dir); !equal(got, want) {
		t.Errorf("downloaded %v, want %v", got, want)
	}
}

func TestDownloadSingle(t *testing.T) {
	f := &fake{objects: map[string]string{"a/plan.json": "plan"}}
	dir := t.TempDir()
	// Into a directory: the object keeps its name.
	if err := Copy(context.Background(), CopyOptions{Source: "gs://b/a/plan.json", Destination: dir}, opener(f)); err != nil {
		t.Fatal(err)
	}
	// To a file name.
	if err := Copy(context.Background(), CopyOptions{Source: "gs://b/a/plan.json", Destination: filepath.Join(dir, "new", "p.json")}, opener(f)); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"plan.json": "plan", "new/p.json": "plan"}
	if got := tree(t, dir); !equal(got, want) {
		t.Errorf("downloaded %v, want %v", got, want)
	}
	if err := Copy(context.Background(), CopyOptions{Source: "gs://b/a/", Destination: dir}, opener(f)); err == nil {
		t.Error("a prefix was copied without --recursive")
	}
}

func TestDownloadRefusesEscapingKeys(t *testing.T) {
	for _, key := range []string{"p/../../evil", "p//etc/passwd"} {
		f := &fake{objects: map[string]string{key: "x"}}
		dir := filepath.Join(t.TempDir(), "dest")
		err := Copy(context.Background(), CopyOptions{Source: "s3://b/p", Destination: dir, Recursive: true}, opener(f))
		if err == nil {
			t.Errorf("key %q was written", key)
		}
		if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); statErr == nil {
			t.Errorf("key %q escaped the destination", key)
		}
	}
}

func TestDownloadLeavesNoPartialFile(t *testing.T) {
	f := &fake{objects: map[string]string{"p/good": "good", "p/bad": "broken"}, failOn: "p/bad"}
	dir := t.TempDir()
	if err := Copy(context.Background(), CopyOptions{Source: "s3://b/p", Destination: dir, Recursive: true}, opener(f)); err == nil {
		t.Fatal("a failed download was reported as success")
	}
	if _, err := os.Stat(filepath.Join(dir, "bad")); !os.IsNotExist(err) {
		t.Error("the half-written file was left behind")
	}
}

func TestUploadRecursive(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"a.txt": "a", "sub/b.txt": "b", "sub/skip.tmp": "no"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &fake{objects: map[string]string{}}
	err := Copy(context.Background(), CopyOptions{Source: dir, Destination: "az://acct/cont/out", Recursive: true, Exclude: []string{"*.tmp"}}, opener(f))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"out/a.txt": "a", "out/sub/b.txt": "b"}
	if !equal(f.objects, want) {
		t.Errorf("uploaded %v, want %v", f.objects, want)
	}
	if err := Copy(context.Background(), CopyOptions{Source: dir, Destination: "az://acct/cont/out"}, opener(f)); err == nil {
		t.Error("a directory was copied without --recursive")
	}
}

func TestUploadSingle(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(file, []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fake{objects: map[string]string{}}
	for _, dest := range []string{"oci://b@ns/x/plan.json", "oci://b@ns/dir/", "oci://b@ns"} {
		if err := Copy(context.Background(), CopyOptions{Source: file, Destination: dest}, opener(f)); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{"x/plan.json": "plan", "dir/plan.json": "plan", "plan.json": "plan"}
	if !equal(f.objects, want) {
		t.Errorf("uploaded %v, want %v", f.objects, want)
	}
}

func TestCopyNeedsOneRemoteSide(t *testing.T) {
	f := &fake{objects: map[string]string{}}
	if err := Copy(context.Background(), CopyOptions{Source: "/a", Destination: "/b"}, opener(f)); err == nil {
		t.Error("a local to local copy was accepted")
	}
	if err := Copy(context.Background(), CopyOptions{Source: "s3://a/x", Destination: "gs://b/x"}, opener(f)); err == nil {
		t.Error("a remote to remote copy was accepted")
	}
}

func TestExcludes(t *testing.T) {
	e, err := compileExcludes([]string{"*.terraform/*", "a?c", "x.y"})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		".terraform/providers/deep/file": true,
		"sub/.terraform/x":               true,
		"main.tf":                        false,
		"abc":                            true,
		"abbc":                           false,
		"x.y":                            true,
		"xzy":                            false, // the dot is not a wildcard
	} {
		if got := e.match(path); got != want {
			t.Errorf("match(%q) = %v, want %v", path, got, want)
		}
	}
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDownloadSetsModTime(t *testing.T) {
	when := time.Date(2020, 5, 6, 7, 8, 9, 0, time.UTC)
	f := &fake{objects: map[string]string{"p/a": "a"}, modTimes: map[string]time.Time{"p/a": when}}
	dir := t.TempDir()
	if err := Copy(context.Background(), CopyOptions{Source: "s3://b/p", Destination: dir, Recursive: true}, opener(f)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(when) {
		t.Errorf("file time = %v, want the object's %v", info.ModTime(), when)
	}
}

func TestSync(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"same": "same", "changed": "new content", "newer": "abcd", "added/file": "x", "skip.log": "local"})
	old := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	f := &fake{
		objects: map[string]string{
			"cache/same":     "same",
			"cache/changed":  "old",
			"cache/newer":    "wxyz", // same size, but the file was written after the object
			"cache/gone":     "x",
			"cache/keep.log": "remote",
			"cachex/other":   "not under the prefix",
		},
		modTimes: map[string]time.Time{"cache/same": future, "cache/changed": future, "cache/newer": old},
	}
	err := Sync(context.Background(), SyncOptions{Source: dir, Destination: "s3://b/cache", Delete: true, Exclude: []string{"*.log"}}, opener(f))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(f.uploads)
	if got := strings.Join(f.uploads, ","); got != "cache/added/file,cache/changed,cache/newer" {
		t.Errorf("uploaded %s", got)
	}
	if got := strings.Join(f.deletes, ","); got != "cache/gone" {
		t.Errorf("deleted %s, want only cache/gone", got)
	}
	want := map[string]string{
		"cache/same": "same", "cache/changed": "new content", "cache/newer": "abcd", "cache/added/file": "x",
		"cache/keep.log": "remote", "cachex/other": "not under the prefix",
	}
	if !equal(f.objects, want) {
		t.Errorf("bucket = %v, want %v", f.objects, want)
	}
}

func TestSyncWithoutDelete(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"a": "a"})
	f := &fake{objects: map[string]string{"p/extra": "x"}}
	if err := Sync(context.Background(), SyncOptions{Source: dir, Destination: "gs://b/p/"}, opener(f)); err != nil {
		t.Fatal(err)
	}
	if len(f.deletes) != 0 || f.objects["p/extra"] != "x" || f.objects["p/a"] != "a" {
		t.Errorf("bucket = %v, deletes = %v", f.objects, f.deletes)
	}
}

func TestSyncRejects(t *testing.T) {
	f := &fake{objects: map[string]string{}}
	dir := t.TempDir()
	if err := Sync(context.Background(), SyncOptions{Source: "s3://b/p", Destination: dir}, opener(f)); err == nil {
		t.Error("a download sync was accepted")
	}
	if err := Sync(context.Background(), SyncOptions{Source: dir, Destination: dir}, opener(f)); err == nil {
		t.Error("a local destination was accepted")
	}
	file := filepath.Join(dir, "f")
	write(t, dir, map[string]string{"f": "x"})
	if err := Sync(context.Background(), SyncOptions{Source: file, Destination: "s3://b/p"}, opener(f)); err == nil {
		t.Error("a file as the source was accepted")
	}
}

func TestUploadFollowsSymlinks(t *testing.T) {
	real := t.TempDir()
	write(t, real, map[string]string{"provider/bin": "binary"})
	dir := t.TempDir()
	write(t, dir, map[string]string{"plain": "p"})
	if err := os.Symlink(filepath.Join(real, "provider"), filepath.Join(dir, "linked")); err != nil {
		t.Skip("no symbolic links here")
	}
	_ = os.Symlink(filepath.Join(real, "missing"), filepath.Join(dir, "dangling"))
	f := &fake{objects: map[string]string{}}
	if err := Sync(context.Background(), SyncOptions{Source: dir, Destination: "s3://b/c"}, opener(f)); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"c/plain": "p", "c/linked/bin": "binary"}
	if !equal(f.objects, want) {
		t.Errorf("bucket = %v, want %v", f.objects, want)
	}
}
