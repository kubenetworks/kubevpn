// Copyright (C) 2015 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build windows
// +build windows

package fs

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const IO_REPARSE_TAG_DEDUP = 0x80000013

func readReparseTag(path string) (uint32, error) {
	namep, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("syscall.UTF16PtrFromString failed with: %s", err)
	}
	attrs := uint32(syscall.FILE_FLAG_BACKUP_SEMANTICS | syscall.FILE_FLAG_OPEN_REPARSE_POINT)
	h, err := syscall.CreateFile(namep, 0, 0, nil, syscall.OPEN_EXISTING, attrs, 0)
	if err != nil {
		return 0, fmt.Errorf("syscall.CreateFile failed with: %s", err)
	}
	defer syscall.CloseHandle(h)

	//https://docs.microsoft.com/windows/win32/api/winbase/ns-winbase-file_attribute_tag_info
	const fileAttributeTagInfo = 9
	type FILE_ATTRIBUTE_TAG_INFO struct {
		FileAttributes uint32
		ReparseTag     uint32
	}

	var ti FILE_ATTRIBUTE_TAG_INFO
	err = windows.GetFileInformationByHandleEx(windows.Handle(h), fileAttributeTagInfo, (*byte)(unsafe.Pointer(&ti)), uint32(unsafe.Sizeof(ti)))
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok && errno == windows.ERROR_INVALID_PARAMETER {
			// It appears calling GetFileInformationByHandleEx with
			// FILE_ATTRIBUTE_TAG_INFO fails on FAT file system with
			// ERROR_INVALID_PARAMETER. Clear ti.ReparseTag in that
			// instance to indicate no symlinks are possible.
			ti.ReparseTag = 0
		} else {
			return 0, fmt.Errorf("windows.GetFileInformationByHandleEx failed with: %s", err)
		}
	}

	return ti.ReparseTag, nil
}

func isDirectoryJunction(reparseTag uint32) bool {
	return reparseTag == windows.IO_REPARSE_TAG_MOUNT_POINT
}

func isDeduplicatedFile(reparseTag uint32) bool {
	return reparseTag == IO_REPARSE_TAG_DEDUP
}

type dirJunctFileInfo struct {
	os.FileInfo
}

func (fi *dirJunctFileInfo) Mode() os.FileMode {
	// Simulate a directory and not a symlink; also set the execute
	// bits so the directory can be traversed Unix-side.
	return fi.FileInfo.Mode() ^ os.ModeSymlink | os.ModeDir | 0111
}

func (fi *dirJunctFileInfo) IsDir() bool {
	return true
}

type dedupFileInfo struct {
	os.FileInfo
}

func (fi *dedupFileInfo) Mode() os.FileMode {
	// A deduplicated file should be treated as a regular file and not an
	// irregular file.
	return fi.FileInfo.Mode() &^ os.ModeIrregular
}

func (f *BasicFilesystem) underlyingLstat(name string) (os.FileInfo, error) {
	var fi, err = os.Lstat(name)

	// There are cases where files are tagged as symlink, but they end up being
	// something else. Make sure we properly handle those types.
	if err == nil {
		// NTFS directory junctions can be treated as ordinary directories,
		// see https://forum.syncthing.net/t/option-to-follow-directory-junctions-symbolic-links/14750
		if fi.Mode()&os.ModeSymlink != 0 && f.junctionsAsDirs {
			if reparseTag, reparseErr := readReparseTag(name); reparseErr == nil && isDirectoryJunction(reparseTag) {
				return &dirJunctFileInfo{fi}, nil
			}
		}

		// Workaround for #9120 till golang properly handles deduplicated files by
		// considering them regular files.
		if fi.Mode()&os.ModeIrregular != 0 {
			if reparseTag, reparseErr := readReparseTag(name); reparseErr == nil && isDeduplicatedFile(reparseTag) {
				return &dedupFileInfo{fi}, nil
			}
		}
	}

	return fi, err
}
