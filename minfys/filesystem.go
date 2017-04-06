// Copyright © 2017 Genome Research Limited
// Author: Sendu Bala <sb10@sanger.ac.uk>.
// The StatFs() code in this file is based on code in
// https://github.com/kahing/goofys Copyright 2015-2017 Ka-Hing Cheung,
// licensed under the Apache License, Version 2.0 (the "License"), stating:
// "You may not use this file except in compliance with the License. You may
// obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0"
//
//  This file is part of wr.
//
//  wr is free software: you can redistribute it and/or modify
//  it under the terms of the GNU Lesser General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  wr is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU Lesser General Public License for more details.
//
//  You should have received a copy of the GNU Lesser General Public License
//  along with wr. If not, see <http://www.gnu.org/licenses/>.

package minfys

// This file implements pathfs.FileSystem methods

import (
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StatFS returns a constant (faked) set of details describing a very large
// file system.
func (fs *MinFys) StatFs(name string) *fuse.StatfsOut {
	const BLOCK_SIZE = uint64(4096)
	const TOTAL_SPACE = uint64(1 * 1024 * 1024 * 1024 * 1024 * 1024) // 1PB
	const TOTAL_BLOCKS = uint64(TOTAL_SPACE / BLOCK_SIZE)
	const INODES = uint64(1 * 1000 * 1000 * 1000) // 1 billion
	const IOSIZE = uint32(1 * 1024 * 1024)        // 1MB
	return &fuse.StatfsOut{
		Blocks: BLOCK_SIZE,
		Bfree:  TOTAL_BLOCKS,
		Bavail: TOTAL_BLOCKS,
		Files:  INODES,
		Ffree:  INODES,
		Bsize:  IOSIZE,
		// NameLen uint32
		// Frsize  uint32
		// Padding uint32
		// Spare   [6]uint32
	}
}

// GetPath combines any base path initially configured in Target with the
// current path, to get the real complete remote path.
func (fs *MinFys) GetPath(relPath string) string {
	return filepath.Join(fs.basePath, relPath)
}

// GetAttr finds out about a given object, returning information from a
// permanent cache if possible. context is not currently used.
func (fs *MinFys) GetAttr(name string, context *fuse.Context) (attr *fuse.Attr, status fuse.Status) {
	if fs.dirs[name] {
		attr = fs.dirAttr
		status = fuse.OK
		return
	}

	var cached bool
	if attr, cached = fs.files[name]; cached {
		status = fuse.OK
		return
	}

	// sequentially check if name is a file or directory. Checking
	// simultaneously doesn't really help since the remote system may queue the
	// requests serially anyway, and it's better to try and minimise requests.
	// We'll use a simple heuristic that if the name contains a '.', it's more
	// likely to be a file.
	if strings.Contains(name, ".") {
		attr, status = fs.maybeFile(name)
		if status != fuse.OK {
			attr, status = fs.maybeDir(name)
		}
	} else {
		attr, status = fs.maybeDir(name)
		if status != fuse.OK {
			attr, status = fs.maybeFile(name)
		}
	}
	return
}

// maybeDir simply calls openDir() and returns the directory attributes if
// 'name' was actually a directory.
func (fs *MinFys) maybeDir(name string) (attr *fuse.Attr, status fuse.Status) {
	_, status = fs.openDir(name)
	if status == fuse.OK {
		attr = fs.dirAttr
	}
	return
}

// maybeFile calls openDir() on the putative file's parent directory, then
// checks to see if that resulted in a file named 'name' being cached.
func (fs *MinFys) maybeFile(name string) (attr *fuse.Attr, status fuse.Status) {
	// rather than call StatObject on name to see if its a file, it's more
	// efficient to try and open it's parent directory and see if that resulted
	// in us caching the file as one of the dir's entries
	parent := filepath.Dir(name)
	if parent == "/" {
		parent = ""
	}
	if _, cached := fs.dirContents[name]; !cached {
		fs.openDir(parent)
		attr, _ = fs.files[name]
	}

	if attr != nil {
		status = fuse.OK
	} else {
		status = fuse.ENOENT
	}
	return
}

// OpenDir gets the contents of the given directory for eg. `ls` purposes. It
// also caches the attributes of all the files within. context is not currently
// used.
func (fs *MinFys) OpenDir(name string, context *fuse.Context) (entries []fuse.DirEntry, status fuse.Status) {
	_, exists := fs.dirs[name]
	if !exists {
		return nil, fuse.ENOENT
	}

	entries, cached := fs.dirContents[name]
	if cached {
		return entries, fuse.OK
	}

	return fs.openDir(name)
}

// openDir gets the contents of the given name, treating it as a directory,
// caching the attributes of its contents.
func (fs *MinFys) openDir(name string) (entries []fuse.DirEntry, status fuse.Status) {
	fullPath := fs.GetPath(name)
	if fullPath != "" {
		fullPath += "/"
	}
	doneCh := make(chan struct{})

	start := time.Now()
	var isDir bool
	attempts := 0
	fs.clientBackoff.Reset()
ATTEMPTS:
	for {
		attempts++
		objectCh := fs.client.ListObjectsV2(fs.bucket, fullPath, false, doneCh)

		for object := range objectCh {
			if object.Err != nil {
				if attempts < fs.maxAttempts {
					<-time.After(fs.clientBackoff.Duration())
					continue ATTEMPTS
				}
				fs.debug("error: ListObjectsV2(%s, %s) call for openDir failed after %d retries and %s: %s", fs.bucket, fullPath, attempts-1, time.Since(start), object.Err)
				status = fuse.EIO
				return
			}
			if object.Key == name {
				continue
			}

			d := fuse.DirEntry{
				Name: object.Key[len(fullPath):],
			}

			fs.mutex.Lock()
			if strings.HasSuffix(d.Name, "/") {
				d.Mode = uint32(fuse.S_IFDIR)
				d.Name = d.Name[0 : len(d.Name)-1]
				fs.dirs[filepath.Join(name, d.Name)] = true
			} else {
				d.Mode = uint32(fuse.S_IFREG)
				thisPath := filepath.Join(name, d.Name)
				mTime := uint64(object.LastModified.Unix())
				attr := &fuse.Attr{
					Mode:  fuse.S_IFREG | fs.fileMode,
					Size:  uint64(object.Size),
					Mtime: mTime,
					Atime: mTime,
					Ctime: mTime,
				}
				fs.files[thisPath] = attr
			}
			fs.mutex.Unlock()

			entries = append(entries, d)
			isDir = true

			// for efficiency, instead of breaking here, we'll keep looping and
			// cache all the dir contents; this does mean we'll never see new
			// entries for this dir in the future
		}
		break
	}
	status = fuse.OK
	fs.debug("info: ListObjectsV2(%s, %s) call for openDir took %s", fs.bucket, fullPath, time.Since(start))

	if isDir {
		fs.mutex.Lock()
		fs.dirs[name] = true
		fs.dirContents[name] = entries
		fs.mutex.Unlock()
	} else {
		entries = nil
		status = fuse.ENOENT
	}

	return
}

// Open is what is called when any request to read a file is made. The file must
// already have been stat'ed (eg. with a GetAttr() call), or we report the file
// doesn't exist. Neither flags nor context are currently used. If CacheData has
// been configured, we defer to openCached(). Otherwise the real implementation
// is in S3File.
func (fs *MinFys) Open(name string, flags uint32, context *fuse.Context) (nodefs.File, fuse.Status) {
	info, exists := fs.files[name]
	if !exists {
		return nil, fuse.ENOENT
	}

	if fs.cacheData {
		return fs.openCached(name, flags, context, info)
	}

	return NewS3File(fs, fs.GetPath(name), info.Size), fuse.OK
}

// openCached downloads the remotePath to the configure CacheDir, then all
// subsequent read/write operations are deferred to the *os.File for that local
// file. Any writes are currently lost because they're not uploaded! NB: there
// is currently no locking, so this should only be called by one process at a
// time (for the same configured CacheDir).
func (fs *MinFys) openCached(name string, flags uint32, context *fuse.Context, info *fuse.Attr) (nodefs.File, fuse.Status) {
	remotePath := fs.GetPath(name)

	// *** will need to do locking to avoid downloading the same file multiple
	// times simultaneously, including by a completely separate process using
	// the same cache dir

	// check cache file doesn't already exist
	var download bool
	dst := filepath.Join(fs.cacheDir, remotePath)
	dstStats, err := os.Stat(dst)
	if err != nil { // don't bother checking os.IsNotExist(err); we'll download based on any error
		os.Remove(dst)
		download = true
	} else {
		// check the file is the right size
		if dstStats.Size() != int64(info.Size) {
			fs.debug("warning: openCached(%s) cached sizes differ: %d local vs %d remote", name, dstStats.Size(), info.Size)
			os.Remove(dst)
			download = true
		}
	}

	if download {
		s := time.Now()
		err = fs.client.FGetObject(fs.bucket, remotePath, dst)
		if err != nil {
			fs.debug("error: FGetObject(%s, %s) call for openCached took %s and failed: %s", fs.bucket, remotePath, time.Since(s), err)
			return nil, fuse.EIO
		}
		dstStats, err := os.Stat(dst)
		if err != nil {
			fs.debug("error: FGetObject(%s, %s) call for openCached took %s and worked, but the downloaded file had error: %s", fs.bucket, remotePath, time.Since(s), err)
			os.Remove(dst)
			return nil, fuse.ToStatus(err)
		} else {
			if dstStats.Size() != int64(info.Size) {
				os.Remove(dst)
				fs.debug("error: FGetObject(%s, %s) call for openCached took %s and worked, but download sizes differ: %d downloaded vs %d remote", fs.bucket, remotePath, time.Since(s), dstStats.Size(), info.Size)
				return nil, fuse.EIO
			}
		}
		fs.debug("info: FGetObject(%s, %s) call for openCached took %s", fs.bucket, remotePath, time.Since(s))
	}

	localFile, err := os.Open(dst)
	if err != nil {
		fs.debug("error: openCached(%s) could not open %s: %s", name, dst, err)
		return nil, fuse.ToStatus(err)
	}

	return nodefs.NewLoopbackFile(localFile), fuse.OK
}