// +build linux

package copy

/*
#include <linux/fs.h>

#ifndef FICLONE
#define FICLONE		_IOW(0x94, 9, int)
#endif
*/
import "C"
import (
	"container/list"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/pkg/pools"
	"github.com/docker/docker/pkg/locker"
	"github.com/docker/docker/pkg/system"
	rsystem "github.com/opencontainers/runc/libcontainer/system"
	"golang.org/x/sys/unix"
)

// Mode indicates whether to use hardlink or copy content
type Mode int

const (
	// Content creates a new file, and copies the content of the file
	Content Mode = iota
	// Hardlink creates a new hardlink to the existing file
	Hardlink
)

const (
	concurrentFileCopies = 8
	fileCopyChannelDepth = 10000
)

func copyRegular(srcPath, dstPath string, fileinfo os.FileInfo, copyWithFileRange, copyWithFileClone *bool) error {
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// If the destination file already exists, we shouldn't blow it away
	dstFile, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileinfo.Mode())
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if *copyWithFileClone {
		_, _, err = unix.Syscall(unix.SYS_IOCTL, dstFile.Fd(), C.FICLONE, srcFile.Fd())
		if err == nil {
			return nil
		}

		*copyWithFileClone = false
		if err == unix.EXDEV {
			*copyWithFileRange = false
		}
	}
	if *copyWithFileRange {
		err = doCopyWithFileRange(srcFile, dstFile, fileinfo)
		// Trying the file_clone may not have caught the exdev case
		// as the ioctl may not have been available (therefore EINVAL)
		if err == unix.EXDEV || err == unix.ENOSYS {
			*copyWithFileRange = false
		} else {
			return err
		}
	}
	return legacyCopy(srcFile, dstFile)
}

func doCopyWithFileRange(srcFile, dstFile *os.File, fileinfo os.FileInfo) error {
	amountLeftToCopy := fileinfo.Size()

	for amountLeftToCopy > 0 {
		n, err := unix.CopyFileRange(int(srcFile.Fd()), nil, int(dstFile.Fd()), nil, int(amountLeftToCopy), 0)
		if err != nil {
			return err
		}

		amountLeftToCopy = amountLeftToCopy - int64(n)
	}

	return nil
}

func legacyCopy(srcFile io.Reader, dstFile io.Writer) error {
	_, err := pools.Copy(dstFile, srcFile)

	return err
}

func copyXattr(srcPath, dstPath, attr string) error {
	data, err := system.Lgetxattr(srcPath, attr)
	if err != nil {
		return err
	}
	if data != nil {
		if err := system.Lsetxattr(dstPath, attr, data, 0); err != nil {
			return err
		}
	}
	return nil
}

func concurrentCopyHelper(copiedFiles map[fileID]string, copiedFilesLocker *locker.Locker, fileToCopy fileCopyInfo, copyXattrs bool, copyWithFileRange, copyWithFileClone *bool) error {
	id := fileID{dev: fileToCopy.stat.Dev, ino: fileToCopy.stat.Ino}

	copiedFilesLocker.Lock(id)
	if otherFileName, ok := copiedFiles[id]; ok {
		copiedFilesLocker.Unlock(id)
		// Make the hardlink, and return
		// it could potentially fail, if the other dstFile
		// had issues copying
		return os.Link(otherFileName, fileToCopy.dstPath)
	}

	if err := copyRegular(fileToCopy.srcPath, fileToCopy.dstPath, fileToCopy.fileInfo, copyWithFileRange, copyWithFileClone); err != nil {
		copiedFilesLocker.Unlock(id)
		return err
	}
	copiedFiles[id] = fileToCopy.dstPath
	copiedFilesLocker.Unlock(id)

	if err := os.Lchown(fileToCopy.dstPath, int(fileToCopy.stat.Uid), int(fileToCopy.stat.Gid)); err != nil {
		return err
	}

	if copyXattrs {
		if err := doCopyXattrs(fileToCopy.srcPath, fileToCopy.dstPath); err != nil {
			return err
		}
	}

	if err := os.Chmod(fileToCopy.dstPath, fileToCopy.fileInfo.Mode()); err != nil {
		return err
	}

	aTime := time.Unix(fileToCopy.stat.Atim.Sec, fileToCopy.stat.Atim.Nsec)
	mTime := time.Unix(fileToCopy.stat.Mtim.Sec, fileToCopy.stat.Mtim.Nsec)
	if err := system.Chtimes(fileToCopy.dstPath, aTime, mTime); err != nil {
		return err
	}
	return nil

}
func concurrentFileCopier(copiedFiles map[fileID]string, copiedFilesLocker *locker.Locker, fileCopyErrorChannel chan error, shutdownChan chan struct{}, filesToCopy chan fileCopyInfo, joiner *sync.WaitGroup, copyXattrs bool) {
	copyWithFileRange := true
	copyWithFileClone := true
	defer joiner.Done()
	for {
		select {
		case <-shutdownChan:
			return
		case fileToCopy, ok := <-filesToCopy:
			if !ok {
				return
			}
			if err := concurrentCopyHelper(copiedFiles, copiedFilesLocker, fileToCopy, copyXattrs, &copyWithFileRange, &copyWithFileClone); err != nil {
				fileCopyErrorChannel <- err
				return
			}
		}
	}
}

type fileCopyInfo struct {
	fileInfo os.FileInfo
	stat     *syscall.Stat_t
	srcPath  string
	dstPath  string
}

type fileID struct {
	dev uint64
	ino uint64
}

type dirMtimeInfo struct {
	dstPath string
	stat    *syscall.Stat_t
}

// DirCopy copies or hardlinks the contents of one directory to another,
// properly handling xattrs, and soft links
//
// Copying xattrs can be opted out of by passing false for copyXattrs.
func DirCopy(srcDir, dstDir string, copyMode Mode, copyXattrs bool) error {

	// This is a map of source file inodes to dst file paths
	copiedFiles := make(map[fileID]string)
	copiedFilesLocker := locker.New()

	dirsToSetMtimes := list.New()
	// This is buffered to handle the case where all copiers hit errors simultaneously
	fileCopyErrorChannel := make(chan error, concurrentFileCopies)
	shutdownChan := make(chan struct{})
	joiner := &sync.WaitGroup{}
	joiner.Add(concurrentFileCopies)
	concurrentFileCopiers := make([]chan fileCopyInfo, concurrentFileCopies)

	for i := range concurrentFileCopiers {
		concurrentFileCopiers[i] = make(chan fileCopyInfo, fileCopyChannelDepth)
		go concurrentFileCopier(copiedFiles, copiedFilesLocker, fileCopyErrorChannel, shutdownChan, concurrentFileCopiers[i], joiner, copyXattrs)
	}

	err := filepath.Walk(srcDir, func(srcPath string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Check if we had an error land on err chan before
		select {
		case err2 := <-fileCopyErrorChannel:
			return err2
		default:
		}

		// Rebase path
		relPath, err := filepath.Rel(srcDir, srcPath)
		if err != nil {
			return err
		}

		dstPath := filepath.Join(dstDir, relPath)
		if err != nil {
			return err
		}

		stat, ok := f.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("Unable to get raw syscall.Stat_t data for %s", srcPath)
		}

		isHardlink := false

		switch f.Mode() & os.ModeType {
		case 0: // Regular file
			if copyMode == Hardlink {
				isHardlink = true
				if err2 := os.Link(srcPath, dstPath); err2 != nil {
					return err2
				}
			} else {
				hash := fnv.New32a()
				if _, err2 := hash.Write([]byte(filepath.Dir(dstPath))); err2 != nil {
					return err2
				}
				idx := int(hash.Sum32()) % len(concurrentFileCopiers)
				select {
				case concurrentFileCopiers[idx] <- fileCopyInfo{srcPath: srcPath, dstPath: dstPath, fileInfo: f, stat: stat}:
				case err2 := <-fileCopyErrorChannel:
					return err2
				}
				return nil
			}
		case os.ModeDir:
			if err := os.Mkdir(dstPath, f.Mode()); err != nil && !os.IsExist(err) {
				return err
			}

		case os.ModeSymlink:
			link, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}

			if err := os.Symlink(link, dstPath); err != nil {
				return err
			}

		case os.ModeNamedPipe:
			fallthrough
		case os.ModeSocket:
			if rsystem.RunningInUserNS() {
				// cannot create a device if running in user namespace
				return nil
			}
			if err := unix.Mkfifo(dstPath, stat.Mode); err != nil {
				return err
			}

		case os.ModeDevice:
			if err := unix.Mknod(dstPath, stat.Mode, int(stat.Rdev)); err != nil {
				return err
			}

		default:
			return fmt.Errorf("unknown file type for %s", srcPath)
		}

		// Everything below is copying metadata from src to dst. All this metadata
		// already shares an inode for hardlinks.
		if isHardlink {
			return nil
		}

		if err := os.Lchown(dstPath, int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}

		if copyXattrs {
			if err := doCopyXattrs(srcPath, dstPath); err != nil {
				return err
			}
		}

		isSymlink := f.Mode()&os.ModeSymlink != 0

		// There is no LChmod, so ignore mode for symlink. Also, this
		// must happen after chown, as that can modify the file mode
		if !isSymlink {
			if err := os.Chmod(dstPath, f.Mode()); err != nil {
				return err
			}
		}

		// system.Chtimes doesn't support a NOFOLLOW flag atm
		// nolint: unconvert
		if f.IsDir() {
			dirsToSetMtimes.PushFront(dirMtimeInfo{dstPath: dstPath, stat: stat})
		} else if !isSymlink {
			aTime := time.Unix(int64(stat.Atim.Sec), int64(stat.Atim.Nsec))
			mTime := time.Unix(int64(stat.Mtim.Sec), int64(stat.Mtim.Nsec))
			if err := system.Chtimes(dstPath, aTime, mTime); err != nil {
				return err
			}
		} else {
			ts := []syscall.Timespec{stat.Atim, stat.Mtim}
			if err := system.LUtimesNano(dstPath, ts); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		close(shutdownChan)
		joiner.Wait()
		return err
	}

	waitChan := make(chan struct{})
	go func() {
		joiner.Wait()
		close(waitChan)
	}()
	for i := range concurrentFileCopiers {
		close(concurrentFileCopiers[i])
	}
	select {
	case <-waitChan:
	case err = <-fileCopyErrorChannel:
		close(shutdownChan)
		joiner.Wait()
		return err
	}

	for e := dirsToSetMtimes.Front(); e != nil; e = e.Next() {
		mtimeInfo := e.Value.(dirMtimeInfo)
		ts := []syscall.Timespec{mtimeInfo.stat.Atim, mtimeInfo.stat.Mtim}
		if err := system.LUtimesNano(mtimeInfo.dstPath, ts); err != nil {
			return err
		}
	}

	return nil
}

func doCopyXattrs(srcPath, dstPath string) error {
	if err := copyXattr(srcPath, dstPath, "security.capability"); err != nil {
		return err
	}

	// We need to copy this attribute if it appears in an overlay upper layer, as
	// this function is used to copy those. It is set by overlay if a directory
	// is removed and then re-created and should not inherit anything from the
	// same dir in the lower dir.
	if err := copyXattr(srcPath, dstPath, "trusted.overlay.opaque"); err != nil {
		return err
	}
	return nil
}
