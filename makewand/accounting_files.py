"""Fixed-directory file capabilities for one accounting transaction.

Operator-selected aliases are resolved once, before acquiring the capability.
Unix operations use dir_fd; Windows pins every ancestor without SHARE_DELETE
and publishes through NtRename relative to the captured parent handle.
"""
import ctypes
import ntpath
import os
import re
import stat
import uuid
from contextlib import contextmanager
from pathlib import Path


def _leaf_name(name):
    if not isinstance(name, str) or not name or name in (".", "..") or any(c in name for c in "/\0"):
        raise ValueError("Accounting file names must be single path components")
    if os.name == "nt":
        if "\\" in name:
            raise ValueError("Accounting file names must be single path components")
        from makewand.windows_paths import _validate_components
        _validate_components(name)
    return name


def _windows_components(path):
    value = os.fspath(path)
    drive, tail = ntpath.splitdrive(value)
    if not tail.startswith("\\") or not (re.fullmatch(r"[A-Za-z]:", drive)
            or (drive.startswith("\\\\") and not drive.startswith(("\\\\?", "\\\\."))
                and len(drive[2:].split("\\")) == 2 and all(drive[2:].split("\\")))):
        raise ValueError("Accounting requires an ordinary drive or UNC share path")
    from makewand.windows_paths import _validate_components
    _validate_components(drive[2:] if drive.startswith("\\\\") else "")
    _validate_components(tail)
    return drive + "\\", [part for part in tail.split("\\") if part]


def _windows_api_path(path):
    root, parts = _windows_components(path)
    value = ntpath.join(root, *parts)
    return "\\\\?\\UNC\\" + value[2:] if value.startswith("\\\\") else "\\\\?\\" + value


def _windows_open(path, *, directory=False, create=False, writable=False, delete=False):
    from makewand import native_windows as native
    kernel = native._api()
    access = (0x20 | 0x80) if directory else (0x80000000 | (0x40000000 if writable else 0))
    # A temporary permits a later DELETE reopen of this same object; locks
    # and ordinary reads deny delete-sharing throughout their lifetimes.
    handle = kernel.CreateFileW(_windows_api_path(path), access, 7 if delete else 3, None,
                               1 if create else 3, 0x00200000 | (0x02000000 if directory else 0), None)
    if handle == ctypes.c_void_p(-1).value:
        raise ctypes.WinError(ctypes.get_last_error())
    try:
        info = native._info(handle)
        if bool(info.attributes & 0x10) != directory or (not directory and info.links != 1):
            raise ValueError("Accounting requires a regular single-link file or directory")
        return handle, (info.volume, info.index_high, info.index_low)
    except BaseException:
        kernel.CloseHandle(handle)
        raise


def _private(fd):
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise ValueError("Accounting state must be a regular file with one link")
    if os.name == "nt":
        from makewand.native_windows import ensure_private_file_descriptor
        ensure_private_file_descriptor(fd)
    else:
        if info.st_uid != os.geteuid():
            raise PermissionError("Accounting state is owned by another account")
        os.fchmod(fd, 0o600)


def _windows_delete_handle(fd):
    import msvcrt
    from ctypes import wintypes
    from makewand import native_windows as native
    kernel = native._api()
    kernel.ReOpenFile.argtypes = [wintypes.HANDLE, wintypes.DWORD, wintypes.DWORD, wintypes.DWORD]
    kernel.ReOpenFile.restype = wintypes.HANDLE
    handle = kernel.ReOpenFile(msvcrt.get_osfhandle(fd), 0x10000 | 0x100 | 0x80, 7, 0)
    if handle == ctypes.c_void_p(-1).value:
        raise ctypes.WinError(ctypes.get_last_error())
    return handle


class AccountingFiles:
    """A canonical ledger name plus a parent capability, valid inside pin()."""

    def __init__(self, path, parent):
        self.path, self.parent = path, parent
        self.name = _leaf_name(path.name)
        self.lock_name = _leaf_name(path.name + ".lock")

    def open(self, name, *, create=False, writable=False, exclusive=False, delete=False):
        name = _leaf_name(name)
        if os.name == "nt":
            import msvcrt
            from makewand import native_windows as native
            try:
                handle, _ = _windows_open(self.path.parent / name, writable=writable, delete=delete)
            except FileNotFoundError:
                if not create:
                    raise
                try:
                    handle, _ = _windows_open(self.path.parent / name, create=True, writable=writable, delete=delete)
                except FileExistsError:
                    if exclusive:
                        raise
                    handle, _ = _windows_open(self.path.parent / name, writable=writable, delete=delete)
            else:
                if exclusive:
                    native._api().CloseHandle(handle)
                    raise FileExistsError(name)
            try:
                fd = msvcrt.open_osfhandle(handle, (os.O_RDWR if writable else os.O_RDONLY) | os.O_BINARY)
            except BaseException:
                native._api().CloseHandle(handle)
                raise
        else:
            flags = (os.O_RDWR if writable else os.O_RDONLY) | os.O_NOFOLLOW | os.O_NONBLOCK
            if create:
                flags |= os.O_CREAT
            if exclusive:
                flags |= os.O_EXCL
            fd = os.open(name, flags, 0o600, dir_fd=self.parent)
        try:
            self.assert_named(name, fd)
            _private(fd)
            self.assert_named(name, fd)
            return fd
        except BaseException:
            os.close(fd)
            raise

    def assert_named(self, name, fd):
        """Refuse an entry switched after opening, without touching its target."""
        if os.name == "nt":
            import msvcrt
            from makewand import native_windows as native
            info = native._info(msvcrt.get_osfhandle(fd))
            handle, current = _windows_open(self.path.parent / _leaf_name(name))
            try:
                if current != (info.volume, info.index_high, info.index_low):
                    raise ValueError("Accounting file identity changed")
            finally:
                native._api().CloseHandle(handle)
        else:
            current = os.stat(_leaf_name(name), dir_fd=self.parent, follow_symlinks=False)
            if not stat.S_ISREG(current.st_mode) or current.st_nlink != 1 or not os.path.samestat(current, os.fstat(fd)):
                raise ValueError("Accounting file identity changed")

    def replace(self, temporary, fd):
        """Publish the already-open temporary, never following the destination."""
        self.assert_named(temporary, fd)
        # Recheck a target inserted after the original read. A symlink/hardlink
        # is refused; tightening happens only through a validated fixed handle.
        try:
            target = self.open(self.name)
        except FileNotFoundError:
            pass
        else:
            os.close(target)
        if os.name == "nt":
            from makewand import native_windows as native

            class RenameInfo(ctypes.Structure):
                _fields_ = [("flags", ctypes.c_uint32), ("root", ctypes.c_void_p),
                            ("name_length", ctypes.c_uint32), ("name", ctypes.c_uint16 * 1)]

            encoded = self.name.encode("utf-16-le")
            buffer = ctypes.create_string_buffer(ctypes.sizeof(RenameInfo) + len(encoded))
            info = RenameInfo.from_buffer(buffer)
            info.flags, info.root, info.name_length = 1, self.parent, len(encoded)
            ctypes.memmove(ctypes.addressof(buffer) + RenameInfo.name.offset, encoded, len(encoded))
            kernel = native._api()
            source = _windows_delete_handle(fd)
            try:
                native._set_nt_file_information(source, 65, buffer, len(buffer))
            finally:
                kernel.CloseHandle(source)
        else:
            os.replace(temporary, self.name, src_dir_fd=self.parent, dst_dir_fd=self.parent)
            os.fsync(self.parent)

    def cleanup(self, name, fd):
        """Remove our temporary object, preserving a substituted entry."""
        if os.name == "nt":
            from makewand import native_windows as native
            handle = _windows_delete_handle(fd)
            try:
                flags = ctypes.c_uint32(1 | 2 | 0x10)  # DELETE | POSIX | IGNORE_READONLY
                native._set_file_information(handle, 21, ctypes.byref(flags), ctypes.sizeof(flags))
            finally:
                native._api().CloseHandle(handle)
        else:
            try:
                self.assert_named(name, fd)
            except (FileNotFoundError, ValueError):
                # POSIX has no unlink-if-inode primitive. Never deliberately
                # remove an entry already observed to belong to somebody else.
                return
            os.unlink(_leaf_name(name), dir_fd=self.parent)

    def save(self, encoded, lock_fd):
        self.assert_named(self.lock_name, lock_fd)
        name = ".call-budget-" + uuid.uuid4().hex
        fd = self.open(name, create=True, writable=True, exclusive=True, delete=True)
        published = False
        try:
            with os.fdopen(fd, "wb", closefd=False) as stream:
                stream.write(encoded)
                stream.flush()
                os.fsync(fd)
            self.assert_named(self.lock_name, lock_fd)
            self.replace(name, fd)
            published = True
        finally:
            try:
                if not published:
                    try:
                        self.cleanup(name, fd)
                    except FileNotFoundError:
                        pass
            finally:
                os.close(fd)


@contextmanager
def pin(value):
    """Resolve legitimate aliases once, then pin/create canonical ancestors."""
    original = Path(value).expanduser()
    if os.name == "nt":
        from makewand.windows_paths import _validate_components
        _validate_components(ntpath.splitdrive(os.fspath(original))[1], navigation=True)
    path = original.resolve()
    _leaf_name(path.name)
    handles = []
    if os.name == "nt":
        from makewand import native_windows as native
        kernel = native._api()
        expected = None
        try:
            try:
                handle, expected = _windows_open(path.parent, directory=True)
            except FileNotFoundError:
                pass
            else:
                handles.append(handle)
            root, parts = _windows_components(path.parent)
            current = root
            handle, _ = _windows_open(current, directory=True)
            handles.append(handle)
            for part in parts:
                current = ntpath.join(current, part)
                try:
                    handle, _ = _windows_open(current, directory=True)
                except FileNotFoundError:
                    try:
                        os.mkdir(_windows_api_path(current), 0o700)
                    except FileExistsError:
                        pass
                    handle, _ = _windows_open(current, directory=True)
                handles.append(handle)
            if expected is not None:
                info = native._info(handles[-1])
                if expected != (info.volume, info.index_high, info.index_low):
                    raise ValueError("Accounting parent identity changed")
            yield AccountingFiles(path, handles[-1])
        finally:
            for handle in reversed(handles):
                kernel.CloseHandle(handle)
    else:
        expected = None
        try:
            expected = os.stat(path.parent, follow_symlinks=False)
        except FileNotFoundError:
            pass
        try:
            flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
            handles.append(os.open(path.anchor, flags))
            for part in path.parent.parts[1:]:
                parent = handles[-1]
                try:
                    os.mkdir(part, 0o700, dir_fd=parent)
                except FileExistsError:
                    pass
                before = os.stat(part, dir_fd=parent, follow_symlinks=False)
                fd = os.open(part, flags, dir_fd=parent)
                handles.append(fd)
                if not stat.S_ISDIR(before.st_mode) or not os.path.samestat(before, os.fstat(fd)):
                    raise ValueError("Accounting ancestor identity changed")
            if expected is not None and not os.path.samestat(expected, os.fstat(handles[-1])):
                raise ValueError("Accounting parent identity changed")
            yield AccountingFiles(path, handles[-1])
        finally:
            for fd in reversed(handles):
                os.close(fd)
