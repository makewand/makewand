//go:build windows

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type applyWindowsPins struct {
	handles []windows.Handle
}

func (p *applyWindowsPins) close() {
	for i := len(p.handles) - 1; i >= 0; i-- {
		_ = windows.CloseHandle(p.handles[i])
	}
}

func validApplyWindowsComponent(name string) bool {
	if name == "" || name == "." || name == ".." || strings.EqualFold(name, ".git") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	for _, character := range name {
		if character < 32 || strings.ContainsRune(`<>:"|?*`, character) {
			return false
		}
	}
	return filepath.IsLocal(name)
}

func validApplyWindowsRelative(name string) bool {
	if !filepath.IsLocal(name) || filepath.Clean(name) != name {
		return false
	}
	for _, component := range strings.Split(name, string(filepath.Separator)) {
		if !validApplyWindowsComponent(component) {
			return false
		}
	}
	return true
}

func openApplyWindowsHandle(path string, directory bool, access uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	// Denying FILE_SHARE_DELETE pins the identity, including a directory's
	// pathname, until every operation using that pathname has completed.
	handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return windows.InvalidHandle, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, fmt.Errorf("apply path must be a non-reparse regular file/directory: %s", path)
	}
	return handle, nil
}

func pinApplyWindowsDirectories(path string) (_ *applyWindowsPins, resultErr error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(abs)
	if len(volume) != 2 || volume[1] != ':' {
		return nil, fmt.Errorf("native apply requires a local Windows drive")
	}
	pins := &applyWindowsPins{}
	defer func() {
		if resultErr != nil {
			pins.close()
		}
	}()
	current := volume + string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(abs[len(volume):], string(filepath.Separator)), string(filepath.Separator))
	for index := -1; index < len(components); index++ {
		if index >= 0 {
			component := components[index]
			if component == "" && len(components) == 1 {
				break
			}
			if !validApplyWindowsComponent(component) {
				return nil, fmt.Errorf("ambiguous Windows apply parent: %s", component)
			}
			current = filepath.Join(current, component)
		}
		handle, err := openApplyWindowsHandle(current, true, windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES)
		if err != nil {
			return nil, err
		}
		pins.handles = append(pins.handles, handle)
	}
	return pins, nil
}

func applyWindowsHandleInfo(handle windows.Handle, path string) (os.FileInfo, error) {
	var duplicate windows.Handle
	process := windows.CurrentProcess()
	if err := windows.DuplicateHandle(process, handle, process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), path)
	defer func() { _ = file.Close() }()
	return file.Stat()
}

func pinApplyWindowsRoot(root *os.Root) (*applyWindowsPins, error) {
	pins, err := pinApplyWindowsDirectories(root.Name())
	if err != nil {
		return nil, err
	}
	anchored, err := root.Stat(".")
	if err != nil {
		pins.close()
		return nil, err
	}
	opened, err := applyWindowsHandleInfo(pins.handles[len(pins.handles)-1], root.Name())
	if err != nil {
		pins.close()
		return nil, err
	}
	if !os.SameFile(anchored, opened) {
		pins.close()
		return nil, fmt.Errorf("windows apply root identity changed")
	}
	return pins, nil
}

// replaceApplyFile uses a handle-relative NTFS rename. Readonly target files
// never need an in-place chmod, so interruption leaves a sealed pre/postimage.
// Unknown reparse aliases, network shares and older unsupported APIs fail closed.
func replaceApplyFile(root *os.Root, source, destination string) error {
	if !validApplyWindowsRelative(source) || !validApplyWindowsRelative(destination) {
		return fmt.Errorf("invalid Windows apply replacement path")
	}
	rootPins, err := pinApplyWindowsRoot(root)
	if err != nil {
		return err
	}
	defer rootPins.close()
	sourcePath, destinationPath := filepath.Join(root.Name(), source), filepath.Join(root.Name(), destination)
	sourcePins, err := pinApplyWindowsDirectories(filepath.Dir(sourcePath))
	if err != nil {
		return err
	}
	defer sourcePins.close()
	destinationPins, err := pinApplyWindowsDirectories(filepath.Dir(destinationPath))
	if err != nil {
		return err
	}
	defer destinationPins.close()
	if handle, err := openApplyWindowsHandle(destinationPath, false, windows.FILE_READ_ATTRIBUTES); err == nil {
		_ = windows.CloseHandle(handle)
	} else if !os.IsNotExist(err) {
		return err
	}
	sourceHandle, err := openApplyWindowsHandle(sourcePath, false, windows.DELETE|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(sourceHandle) }()

	type renameInformation struct {
		Flags      uint32
		Root       windows.Handle
		NameLength uint32
		Name       [1]uint16
	}
	name, err := windows.UTF16FromString(filepath.Base(destination))
	if err != nil {
		return err
	}
	name = name[:len(name)-1]
	// A local Windows filename component contains at most 255 UTF-16 units.
	// Check before narrowing the native FileNameLength and buffer-size fields.
	if len(name) > 255 {
		return fmt.Errorf("windows apply filename exceeds component limit")
	}
	//nolint:gosec // G115: the UTF-16 component length is bounded to 255 above.
	nameLength := uint32(len(name)) * 2
	bufferSize := uint32(unsafe.Sizeof(renameInformation{})) + nameLength
	buffer := make([]byte, bufferSize)
	header := (*renameInformation)(unsafe.Pointer(&buffer[0]))
	header.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_IGNORE_READONLY_ATTRIBUTE
	header.Root = destinationPins.handles[len(destinationPins.handles)-1]
	header.NameLength = nameLength
	names := unsafe.Slice((*uint16)(unsafe.Add(unsafe.Pointer(&buffer[0]), unsafe.Offsetof(renameInformation{}.Name))), len(name))
	copy(names, name)
	return windows.SetFileInformationByHandle(sourceHandle, windows.FileRenameInfoEx, &buffer[0], bufferSize)
}

func removeApplyFile(root *os.Root, path string) error {
	if !validApplyWindowsRelative(path) {
		return fmt.Errorf("invalid Windows apply removal path")
	}
	rootPins, err := pinApplyWindowsRoot(root)
	if err != nil {
		return err
	}
	defer rootPins.close()
	absolute := filepath.Join(root.Name(), path)
	parents, err := pinApplyWindowsDirectories(filepath.Dir(absolute))
	if err != nil {
		return err
	}
	defer parents.close()
	handle, err := openApplyWindowsHandle(absolute, false, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE)
	return windows.SetFileInformationByHandle(handle, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)))
}
