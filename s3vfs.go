package s3vfs

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"time"

	"golang.org/x/tools/godoc/vfs"

	"strings"

	"github.com/sqs/s3"
	"github.com/sqs/s3/s3util"
	"sourcegraph.com/sourcegraph/rwvfs"
)

var DefaultS3Config = s3util.Config{
	Keys: &s3.Keys{
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_KEY"),
	},
	Service: s3.DefaultService,
}

// S3 returns an implementation of FileSystem using the specified S3 bucket and
// config. If config is nil, DefaultS3Config is used.
//
// The bucket URL is the full URL to the bucket on Amazon S3, including the
// bucket name and AWS region (e.g.,
// https://s3-us-west-2.amazonaws.com/mybucket).
func S3(bucket *url.URL, config *s3util.Config) rwvfs.FileSystem {
	if config == nil {
		config = &DefaultS3Config
	}
	return &S3FS{bucket, config}
}

type S3FS struct {
	bucket *url.URL
	config *s3util.Config
}

func (fs *S3FS) String() string {
	return fmt.Sprintf("S3 filesystem at %s", fs.bucket)
}

func (fs *S3FS) url(path string) string {
	path = pathpkg.Join(fs.bucket.Path, path)
	return fs.bucket.ResolveReference(&url.URL{Path: path}).String()
}

func (fs *S3FS) Open(name string) (vfs.ReadSeekCloser, error) {
	return fs.open(name, "")
}

type rangeTransport struct {
	http.RoundTripper
	rangeVal string
}

func (t rangeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = cloneRequest(req)
	req.Header.Set("range", t.rangeVal)

	transport := t.RoundTripper
	if transport == nil {
		transport = http.DefaultTransport
	}

	resp, err := transport.RoundTrip(req)
	if resp != nil && resp.StatusCode == http.StatusPartialContent {
		resp.StatusCode = http.StatusOK
	}
	return resp, err
}

// cloneRequest returns a clone of the provided *http.Request. The clone is a
// shallow copy of the struct and its Header map.
func cloneRequest(r *http.Request) *http.Request {
	// shallow copy of the struct
	r2 := new(http.Request)
	*r2 = *r
	// deep copy of the Header
	r2.Header = make(http.Header)
	for k, s := range r.Header {
		r2.Header[k] = s
	}
	return r2
}

func (fs *S3FS) open(name string, rangeHeader string) (vfs.ReadSeekCloser, error) {
	cfg := fs.config
	if rangeHeader != "" {
		tmp := *cfg
		cfg = &tmp
		var existingTransport http.RoundTripper
		if cfg.Client != nil {
			existingTransport = cfg.Client.Transport
		}
		cfg.Client = &http.Client{Transport: rangeTransport{RoundTripper: existingTransport, rangeVal: rangeHeader}}
	}

	rdr, err := s3util.Open(fs.url(name), cfg)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: fs.url(name), Err: err}
	}

	b, err := ioutil.ReadAll(rdr)
	if err != nil {
		return nil, err
	}
	defer rdr.Close()
	return nopCloser{bytes.NewReader(b)}, nil
}

func (fs *S3FS) OpenFetcher(name string) (vfs.ReadSeekCloser, error) {
	return &explicitFetchFile{name: name, fs: fs, autofetch: true}, nil
}

type explicitFetchFile struct {
	name               string
	fs                 *S3FS
	startByte, endByte int64
	rc                 vfs.ReadSeekCloser
	autofetch          bool
}

var vlog = log.New(ioutil.Discard, "s3vfs: ", 0)

func (f *explicitFetchFile) Read(p []byte) (n int, err error) {
	ofs, err := f.Seek(0, 1) // get current offset
	if err != nil {
		return 0, err
	}
	if start, end := ofs, ofs+int64(len(p)); !f.isFetched(start, end) {
		if !f.autofetch {
			return 0, fmt.Errorf("s3vfs: range %d-%d not fetched (%d-%d fetched; offset %d)", start, end, f.startByte, f.endByte, ofs)
		}
		const x = 4 // overfetch factor (because network RTT >> network throughput)
		fetchEnd := end + (end-start)*x
		vlog.Printf("Autofetching range %d-%d because read of unfetched %d-%d attempted (%d bytes)", start, fetchEnd, start, end, len(p))
		if err := f.Fetch(start, fetchEnd); err != nil {
			return 0, err
		}
	}
	return f.rc.Read(p)
}

func (f *explicitFetchFile) isFetched(start, end int64) bool {
	return f.rc != nil && start <= end && start >= f.startByte && end <= f.endByte
}

func (f *explicitFetchFile) Fetch(start, end int64) error {
	if f.isFetched(start, end) {
		// Already prefetched.
		vlog.Printf("Already fetched %d-%d (fetched range is %d-%d)", start, end, f.startByte, f.endByte)
		return nil
	}

	// Close existing open reader (if any).
	if err := f.Close(); err != nil {
		return err
	}

	rng := fmt.Sprintf("bytes=%d-%d", start, end)
	var err error
	f.rc, err = f.fs.open(f.name, rng)
	if err == nil {
		f.startByte = start
		f.endByte = end
	}
	return err
}

var errRelOfs = errors.New("s3vfs: seek to offset relative to end of file is not supported")

func (f *explicitFetchFile) Seek(offset int64, whence int) (int64, error) {
	if f.rc == nil {
		return 0, errors.New("s3vfs: must call Fetch before Seek")
	}

	switch whence {
	case 0:
		offset -= f.startByte
	case 2:
		return 0, errRelOfs
	}
	n, err := f.rc.Seek(offset, whence)
	n += f.startByte
	return n, err
}

func (f *explicitFetchFile) Close() error {
	if f.rc != nil {
		err := f.rc.Close()
		f.rc = nil
		f.startByte = 0
		f.endByte = 0
		return err
	}
	return nil
}

func (fs *S3FS) ReadDir(path string) ([]os.FileInfo, error) {
	dir, err := s3util.NewFile(fs.url(path), fs.config)
	if err != nil {
		return nil, &os.PathError{Op: "readdir", Path: fs.url(path), Err: err}
	}

	fis, err := dir.Readdir(0)
	if err != nil {
		return nil, err
	}
	for i, fi := range fis {
		fis[i] = &fileInfo{
			name:    pathpkg.Base(fi.Name()),
			size:    fi.Size(),
			mode:    fi.Mode(),
			modTime: fi.ModTime(),
			sys:     fi.Sys(),
		}
	}
	return fis, nil
}

func (fs *S3FS) Lstat(name string) (os.FileInfo, error) {
	fi, err := fs.lstat(name)
	if err != nil {
		return nil, &os.PathError{Op: "lstat", Path: fs.url(name), Err: err}
	}
	return fi, nil
}

func (fs *S3FS) lstat(name string) (os.FileInfo, error) {
	name = strings.TrimPrefix(filepath.Clean(name), "/")

	if name == "." {
		return &fileInfo{
			name:    ".",
			size:    0,
			mode:    os.ModeDir,
			modTime: time.Time{},
		}, nil
	}

	client := fs.config.Client
	if client == nil {
		client = http.DefaultClient
	}

	q := make(url.Values)
	q.Set("prefix", name+"/")
	q.Set("max-keys", "1")
	u := fs.bucket.ResolveReference(&url.URL{RawQuery: q.Encode()})

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	fs.config.Sign(req, *fs.config.Keys)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, newRespError(resp)
	}

	result := struct{ Contents []struct{ Key string } }{}
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if err := resp.Body.Close(); err != nil {
		return nil, err
	}

	// If Contents is non-empty, then this is a dir.
	if len(result.Contents) == 1 {
		return &fileInfo{
			name: name,
			size: 0,
			mode: os.ModeDir,
		}, nil
	}

	// Otherwise, see if a key exists here.
	req, err = http.NewRequest("HEAD", fs.url(name), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	fs.config.Sign(req, *fs.config.Keys)
	resp, err = client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, os.ErrNotExist
	} else if resp.StatusCode != 200 {
		return nil, newRespError(resp)
	}
	t, _ := time.Parse(http.TimeFormat, resp.Header.Get("last-modified"))
	return &fileInfo{
		name:    name,
		size:    resp.ContentLength,
		mode:    0, // file
		modTime: t,
	}, nil
}

func (fs *S3FS) Stat(name string) (os.FileInfo, error) {
	return fs.Lstat(name)
}

// Create opens the file at path for writing, creating the file if it doesn't
// exist and truncating it otherwise.
func (fs *S3FS) Create(path string) (io.WriteCloser, error) {
	wc, err := s3util.Create(fs.url(path), nil, fs.config)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: fs.url(path), Err: err}
	}
	return wc, nil
}

func (fs *S3FS) Mkdir(name string) error {
	// S3 doesn't have directories.
	return nil
}

// MkdirAll implements rwvfs.MkdirAllOverrider.
func (fs *S3FS) MkdirAll(name string) error {
	// S3 doesn't have directories.
	return nil
}

func (fs *S3FS) Remove(name string) (err error) {
	var rdr io.ReadCloser
	rdr, err = s3util.Delete(fs.url(name), fs.config)
	defer func() {
		if rdr != nil {
			err2 := rdr.Close()
			if err == nil {
				err = err2
			}
		}
	}()
	return err
}

type nopCloser struct {
	io.ReadSeeker
}

func (nc nopCloser) Close() error { return nil }

type fileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	sys     interface{}
}

func (f *fileInfo) Name() string       { return f.name }
func (f *fileInfo) Size() int64        { return f.size }
func (f *fileInfo) Mode() os.FileMode  { return f.mode }
func (f *fileInfo) ModTime() time.Time { return f.modTime }
func (f *fileInfo) IsDir() bool        { return f.mode&os.ModeDir != 0 }
func (f *fileInfo) Sys() interface{}   { return f.sys }

type respError struct {
	r *http.Response
	b bytes.Buffer
}

func newRespError(r *http.Response) *respError {
	e := new(respError)
	e.r = r
	io.Copy(&e.b, r.Body)
	r.Body.Close()
	return e
}

func (e *respError) Error() string {
	return fmt.Sprintf(
		"unwanted http status %d: %q",
		e.r.StatusCode,
		e.b.String(),
	)
}
