package download

import (
	"os"
	"syscall"
)

func openStagingFile(path string, flag int) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	mode := uint32(syscall.OPEN_EXISTING)
	if flag&os.O_CREATE != 0 {
		mode = syscall.OPEN_ALWAYS
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, mode,
		syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info syscall.ByHandleFileInformation
	err = syscall.GetFileInformationByHandle(h, &info)
	if err == nil && info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		err = syscall.ELOOP
	}
	if err != nil {
		syscall.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
