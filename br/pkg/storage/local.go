// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package storage

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/logutil"
	"go.uber.org/zap"
)

const (
	localDirPerm  os.FileMode = 0o777
	localFilePerm os.FileMode = 0o644
	// LocalURIPrefix represents the local storage prefix.
	LocalURIPrefix = "file://"
)

// LocalStorage represents local file system storage.
//
// export for using in tests.
type LocalStorage struct {
	base string
	// Whether ignoring ENOINT while deleting.
	// Don't fail when deleting an unexist file is more like
	// a normal ExternalStorage implementation does.
	IgnoreEnoentForDelete bool
}

// Base returns the base dir used by this local storage.
func (l *LocalStorage) Base() string {
	return l.base
}

// DeleteFile deletes the file.
func (l *LocalStorage) DeleteFile(_ context.Context, name string) error {
	failpoint.Inject("local_delete_file_err", func(v failpoint.Value) {
		failpoint.Return(errors.New(v.(string)))
	})
	path := filepath.Join(l.base, name)
	err := os.Remove(path)
	if err != nil &&
		l.IgnoreEnoentForDelete &&
		os.IsNotExist(err) {
		return nil
	}
	return errors.Annotatef(err, "failed to delete file %v", name)
}

// DeleteFiles deletes the files.
func (l *LocalStorage) DeleteFiles(ctx context.Context, names []string) error {
	for _, name := range names {
		err := l.DeleteFile(ctx, name)
		if err != nil {
			return err
		}
	}
	return nil
}

// WriteFile writes data to a file to storage.
func (l *LocalStorage) WriteFile(_ context.Context, name string, data []byte) error {
	failpoint.Inject("local_write_file_err", func(v failpoint.Value) {
		failpoint.Return(errors.New(v.(string)))
	})

	// because `os.WriteFile` is not atomic, directly write into it may reset the file
	// to an empty file if write is not finished.
	tmpPath := filepath.Join(l.base, name) + ".tmp." + uuid.NewString()
	if err := os.WriteFile(tmpPath, data, localFilePerm); err != nil {
		path := filepath.Dir(tmpPath)
		log.Info("failed to write file, try to mkdir the path", zap.String("path", path))
		exists, existErr := pathExists(path)
		if existErr != nil {
			return errors.Annotatef(err, "after failed to write file, failed to check path exists : %v", existErr)
		}
		if exists {
			return errors.Trace(err)
		}
		if mkdirErr := mkdirAll(path); mkdirErr != nil {
			return errors.Annotatef(err, "after failed to write file, failed to mkdir : %v", mkdirErr)
		}
		if err := os.WriteFile(tmpPath, data, localFilePerm); err != nil {
			return errors.Trace(err)
		}
	}
	if err := os.Rename(tmpPath, filepath.Join(l.base, name)); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// ReadFile reads the file from the storage and returns the contents.
func (l *LocalStorage) ReadFile(_ context.Context, name string) ([]byte, error) {
	path := filepath.Join(l.base, name)
	return os.ReadFile(path)
}

// FileExists implement ExternalStorage.FileExists.
func (l *LocalStorage) FileExists(_ context.Context, name string) (bool, error) {
	path := filepath.Join(l.base, name)
	return pathExists(path)
}

// WalkDir traverse all the files in a dir.
//
// fn is the function called for each regular file visited by WalkDir.
// The first argument is the file path that can be used in `Open`
// function; the second argument is the size in byte of the file determined
// by path.
func (l *LocalStorage) WalkDir(_ context.Context, opt *WalkOption, fn func(string, int64) error) error {
	if opt == nil {
		opt = &WalkOption{}
	}
	base := filepath.Join(l.base, opt.SubDir)
	return filepath.Walk(base, func(path string, f os.FileInfo, err error) error {
		if os.IsNotExist(err) {
			log.Info("Local Storage Hint: WalkDir yields a tomestone, a race may happen.", zap.String("path", path))
			if !opt.IncludeTombstone {
				// if path not exists and the client doesn't require its tombstone,
				// we should return nil to continue.
				return nil
			}
			relativeToBase, err := filepath.Rel(base, path)
			if err != nil {
				log.Panic("filepath.Walk returns a path that isn't a subdir of the base dir.",
					zap.String("path", path), zap.String("base", base), logutil.ShortError(err))
			}
			if !strings.HasPrefix(relativeToBase, opt.ObjPrefix) {
				return nil
			}

			// Convert to relative path from l.base for consistency with cloud storage
			path, err = filepath.Rel(l.base, path)
			if err != nil {
				log.Panic("filepath.Walk returns a path that isn't a subdir of the base dir.",
					zap.String("path", path), zap.String("base", l.base), logutil.ShortError(err))
			}
			// NOTE: This may cause a tombstone of the dir emit to the caller when
			// call `Walk` in a non-exist dir.
			return fn(path, TombstoneSize)
		}
		if err != nil {
			return errors.Trace(err)
		}

		if f == nil {
			return nil
		}
		if f.IsDir() {
			// walk will call this for base itself.
			if path != base && opt.SkipSubDir {
				return filepath.SkipDir
			}
			return nil
		}
		// in mac osx, the path parameter is absolute path; in linux, the path is relative path to execution base dir,
		// so use Rel to convert to relative path to the directory being walked (base)
		relativeToBase, _ := filepath.Rel(base, path)
		if !strings.HasPrefix(relativeToBase, opt.ObjPrefix) {
			return nil
		}

		// Convert to relative path from l.base for consistency with cloud storage
		path, _ = filepath.Rel(l.base, path)

		size := f.Size()
		// if not a regular file, we need to use os.stat to get the real file size
		if !f.Mode().IsRegular() {
			stat, err := os.Stat(filepath.Join(l.base, path))
			if err != nil {
				// error may happen because of file deleted after walk started, or other errors
				// like #49423. We just return 0 size and let the caller handle it in later
				// logic.
				log.Warn("failed to get file size", zap.String("path", path), zap.Error(err))
				return fn(path, 0)
			}
			size = stat.Size()
		}
		return fn(path, size)
	})
}

// URI returns the base path as an URI with a file:/// prefix.
func (l *LocalStorage) URI() string {
	return LocalURIPrefix + l.base
}

// Open a Reader by file path, path is a relative path to base path.
func (l *LocalStorage) Open(_ context.Context, path string, o *ReaderOption) (ExternalFileReader, error) {
	//nolint: gosec
	f, err := os.Open(filepath.Join(l.base, path))
	if err != nil {
		return nil, errors.Trace(err)
	}
	pos, endPos := int64(0), int64(-1)
	if o != nil {
		if o.EndOffset != nil {
			endPos = *o.EndOffset
		}
		if o.StartOffset != nil {
			_, err = f.Seek(*o.StartOffset, io.SeekStart)
			if err != nil {
				return nil, errors.Trace(err)
			}
			pos = *o.StartOffset
		}
	}
	return &localFile{File: f, pos: pos, endPos: endPos}, nil
}

type localFile struct {
	*os.File
	pos    int64
	endPos int64
}

func (f *localFile) Read(p []byte) (n int, err error) {
	if f.endPos == -1 {
		return f.File.Read(p)
	}

	pEnd := f.endPos - f.pos
	if pEnd <= 0 {
		return 0, io.EOF
	}
	if pEnd > int64(len(p)) {
		pEnd = int64(len(p))
	}
	p = p[:pEnd]
	n, err = f.File.Read(p)
	f.pos += int64(n)
	return n, err
}

func (f *localFile) Seek(offset int64, whence int) (int64, error) {
	n, err := f.File.Seek(offset, whence)
	if err != nil {
		return 0, errors.Trace(err)
	}
	f.pos, _ = f.File.Seek(0, io.SeekCurrent)
	return n, nil
}

func (f *localFile) GetFileSize() (int64, error) {
	stat, err := f.Stat()
	if err != nil {
		return 0, errors.Trace(err)
	}
	return stat.Size(), nil
}

// Create implements ExternalStorage interface.
func (l *LocalStorage) Create(_ context.Context, name string, _ *WriterOption) (ExternalFileWriter, error) {
	filename := filepath.Join(l.base, name)
	dir := filepath.Dir(filename)
	err := os.MkdirAll(dir, 0750)
	if err != nil {
		return nil, errors.Trace(err)
	}
	file, err := os.Create(filename)
	if err != nil {
		return nil, errors.Trace(err)
	}
	buf := bufio.NewWriter(file)
	return newFlushStorageWriter(buf, buf, file), nil
}

// Rename implements ExternalStorage interface.
func (l *LocalStorage) Rename(_ context.Context, oldFileName, newFileName string) error {
	return errors.Trace(os.Rename(filepath.Join(l.base, oldFileName), filepath.Join(l.base, newFileName)))
}

// Close implements ExternalStorage interface.
func (*LocalStorage) Close() {}

func (l *LocalStorage) CopyFrom(ctx context.Context, e ExternalStorage, spec CopySpec) error {
	sl, ok := e.(*LocalStorage)
	if !ok {
		return errors.Annotatef(berrors.ErrInvalidArgument, "expect source to be LocalStorage, got %T", e)
	}
	from := filepath.Join(sl.base, spec.From)
	to := filepath.Join(l.base, spec.To)
	if err := mkdirAll(filepath.Dir(to)); err != nil {
		return errors.Trace(err)
	}
	return os.Link(from, to)
}

func pathExists(_path string) (bool, error) {
	_, err := os.Stat(_path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, errors.Trace(err)
	}
	return true, nil
}

// NewLocalStorage return a LocalStorage at directory `base`.
//
// export for test.
func NewLocalStorage(base string) (*LocalStorage, error) {
	ok, err := pathExists(base)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if !ok {
		err := mkdirAll(base)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	return &LocalStorage{base: base}, nil
}
